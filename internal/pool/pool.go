// Package pool implements COMP-010: a bounded worker pool / backpressure layer
// in front of the composed provider-call seam (server.InferFunc, ADR-0006).
//
// It caps the number of concurrent UPSTREAM calls at a configurable limit
// (GATEWAY_WORKER_POOL_SIZE, default 100 — ADR-0003) using a buffered-channel
// semaphore. The acquire is NON-BLOCKING: when every slot is taken, the guard
// rejects the request IMMEDIATELY with a typed sentinel (ErrPoolSaturated)
// WITHOUT starting a goroutine and WITHOUT blocking the caller (AC-038,
// INV-001, POL-003). The caller (the server boundary) maps that sentinel to
// HTTP 503 + Retry-After, reusing the same classification path as the
// resilience layer's open-breaker/deadline fast-fail.
//
// Composition (ADR-0006), built in cmd/gateway:
//
//	pool.Guard(size, retryAfter, resilience.InferFunc())
//	  → pool acquire (reject-before-work)
//	    → retry( breaker.Execute( provider.Infer ) )
//
// The pool sits at the entry of the provider-call path so excess load is shed
// before any retry/breaker/provider work is started. Because Guard preserves
// the server.InferFunc signature, CARD-005's rate-limit middleware can sit
// OUTSIDE this layer (an earlier/outer layer) without any change here.
//
// Invariants:
//   - The number of goroutines blocked on the upstream path never exceeds the
//     pool limit (NFR-006 / AC-047): a slot is held for the entire wrapped call
//     and released exactly once on return (success or error).
//   - No goroutine is leaked on rejection: a rejected request returns on the
//     caller's own goroutine without spawning anything (NFR-003 / AC-044).
package pool

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/server"
)

// ErrPoolSaturated is the typed sentinel returned when an acquire fails because
// every slot is in use. It is classified WITHOUT string-matching: the server
// maps it to HTTP 503 (via errors.Is against server.ErrServiceUnavailable) and
// surfaces the Retry-After hint (via the RetryAfter method), consistent with
// the resilience 503 fast-fail path (AC-038, INV-001).
//
// Match it directly with errors.Is(err, ErrPoolSaturated), or classify it as a
// service-unavailable signal with errors.Is(err, server.ErrServiceUnavailable).
var ErrPoolSaturated = errors.New("pool: worker pool saturated")

// saturatedError is the concrete error the guard returns on rejection. It wraps
// ErrPoolSaturated (so errors.Is matches the sentinel) and additionally matches
// server.ErrServiceUnavailable (so the server's existing 503 mapping picks it
// up without importing this package), and carries the Retry-After hint.
type saturatedError struct {
	retryAfter time.Duration
}

// Error implements error.
func (e *saturatedError) Error() string { return ErrPoolSaturated.Error() }

// Unwrap exposes ErrPoolSaturated so errors.Is(err, ErrPoolSaturated) matches.
func (e *saturatedError) Unwrap() error { return ErrPoolSaturated }

// Is reports a match against server.ErrServiceUnavailable so the server maps
// the saturation rejection to HTTP 503 via errors.Is, the same way it handles
// the resilience layer's Unavailable (no import of this package required).
func (e *saturatedError) Is(target error) bool {
	return target == server.ErrServiceUnavailable
}

// RetryAfter returns the Retry-After hint for the 503 response header. It
// satisfies the server's retryAfterer interface so the header is set on the
// shed-load 503 exactly as on the resilience fast-fail (AC-038).
func (e *saturatedError) RetryAfter() time.Duration { return e.retryAfter }

// Pool is the bounded worker pool. It holds a buffered-channel semaphore whose
// capacity is the concurrency limit, plus the Retry-After hint surfaced on a
// saturation rejection. It is injected from config (no globals); construct it
// with New.
type Pool struct {
	// sem is the semaphore: capacity == limit. A token is sent to acquire a
	// slot and received to release it. A non-blocking send (select/default)
	// makes acquire reject immediately when the channel is full.
	sem chan struct{}
	// retryAfter is the hint surfaced via Retry-After on a saturation 503.
	retryAfter time.Duration
}

// New constructs a Pool capping concurrent upstream calls at size
// (GATEWAY_WORKER_POOL_SIZE, ADR-0003). size must be > 0; New panics on a
// non-positive size because config validation already guarantees it (fail-loud
// at boot rather than silently unbounded). retryAfter is the Retry-After hint
// for the shed-load 503 (typically config.Breaker.RetryAfter, shared with the
// resilience path).
func New(size int, retryAfter time.Duration) *Pool {
	if size <= 0 {
		panic("pool: size must be > 0")
	}
	return &Pool{
		sem:        make(chan struct{}, size),
		retryAfter: retryAfter,
	}
}

// Limit reports the configured concurrency limit (the semaphore capacity).
func (p *Pool) Limit() int { return cap(p.sem) }

// InFlight reports the number of slots currently held. It is a point-in-time
// snapshot, primarily for tests/observability.
func (p *Pool) InFlight() int { return len(p.sem) }

// tryAcquire attempts a NON-BLOCKING reservation of a slot. It returns true and
// holds the slot when one was free, or false IMMEDIATELY when the pool is full
// (no blocking, no goroutine). Release the slot with release.
func (p *Pool) tryAcquire() bool {
	select {
	case p.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// release frees a previously-acquired slot. It must be called exactly once per
// successful tryAcquire, so a freed slot immediately accepts new work (AC-039).
func (p *Pool) release() { <-p.sem }

// Guard wraps next with the bounded-pool backpressure. The returned
// server.InferFunc:
//
//   - On a free slot: holds it, runs next (retry→breaker→provider), and
//     releases the slot on return (success or error). The slot is held for the
//     full duration of the wrapped call so concurrency never exceeds the limit
//     (NFR-006 / AC-047).
//   - On a full pool: returns &saturatedError IMMEDIATELY — no goroutine
//     started, no blocking — which the server maps to 503 + Retry-After
//     (AC-038, INV-001).
//
// The signature is identical to next, keeping the seam unchanged so outer
// layers (rate-limit, CARD-005) compose without modification.
func (p *Pool) Guard(next server.InferFunc) server.InferFunc {
	return func(ctx context.Context, prov provider.Provider, req provider.Request) (provider.Response, error) {
		if !p.tryAcquire() {
			// Reject-before-work: shed load without spawning a goroutine.
			return provider.Response{}, &saturatedError{retryAfter: p.retryAfter}
		}
		// Slot held; ensure it is released exactly once regardless of outcome
		// (including a panic in next, which must not leak a slot).
		defer p.release()
		return next(ctx, prov, req)
	}
}

// Guard is a package-level convenience that builds a Pool from size/retryAfter
// and wraps next in one call, for the common cmd/gateway wiring. Prefer the
// method form (New + (*Pool).Guard) when the *Pool is needed for observation
// (Limit/InFlight) or to share one pool across several seams.
func Guard(size int, retryAfter time.Duration, next server.InferFunc) server.InferFunc {
	return New(size, retryAfter).Guard(next)
}

// GuardStream wraps next (a streaming initiation seam) with the SAME bounded
// pool so streams count against the concurrency limit (CARD-014, NFR-006). The
// returned server.StreamFunc:
//
//   - On a free slot: holds it, INITIATES the stream via next (breaker →
//     provider.InferStream). On an initiation ERROR the slot is released
//     immediately (no leak) and the error is propagated so the server fast-fails
//     (503 for an open breaker, 502 otherwise) BEFORE any SSE byte. On SUCCESS
//     the slot is held for the WHOLE stream lifetime and released EXACTLY ONCE
//     when the stream ends — channel closed, ctx cancelled, or error — via a
//     forwarding goroutine that drains the source channel to completion.
//   - On a full pool: returns &saturatedError IMMEDIATELY — no goroutine
//     started, no blocking — which the server maps to 503 + Retry-After
//     (AC-014b), exactly like the unary Guard.
//
// Slot/goroutine safety (INV-001, NFR-006): the slot is released exactly once
// (a sync.Once guards a double-release if the source closes AND ctx cancels) and
// the forwarding goroutine always terminates — it returns when the source
// channel closes, or when ctx is done (it then keeps draining the source in a
// detached tail so the provider goroutine can finish and close, never blocking
// on an unread send). No slot leak, no goroutine leak under cancellation.
func (p *Pool) GuardStream(next server.StreamFunc) server.StreamFunc {
	return func(ctx context.Context, prov provider.Provider, req provider.Request) (<-chan provider.Chunk, error) {
		if !p.tryAcquire() {
			// Reject-before-work: shed load without spawning a goroutine.
			return nil, &saturatedError{retryAfter: p.retryAfter}
		}

		src, err := next(ctx, prov, req)
		if err != nil {
			// Initiation failed: release the slot now (the stream never started)
			// and propagate so the server fast-fails before committing to 200.
			p.release()
			return nil, err
		}

		// Initiation succeeded: hold the slot for the stream lifetime. A forwarding
		// goroutine copies src → out and releases the slot exactly once when src is
		// fully drained (closed). releaseOnce guards against any future double-call.
		var releaseOnce sync.Once
		releaseSlot := func() { releaseOnce.Do(p.release) }

		out := make(chan provider.Chunk)
		go func() {
			defer close(out)
			defer releaseSlot()
			for {
				select {
				case chunk, ok := <-src:
					if !ok {
						// Source drained/closed: stream ended; slot released by defer.
						return
					}
					select {
					case out <- chunk:
					case <-ctx.Done():
						// Consumer gone (client disconnect): stop forwarding, but keep
						// draining src so the provider goroutine can finish and close it
						// — otherwise its blocked send would leak. Slot released by defer
						// once src closes.
						drain(src)
						return
					}
				case <-ctx.Done():
					// Cancelled while waiting for the next chunk: drain src to its close
					// so the provider goroutine never leaks, then return (slot released
					// by defer).
					drain(src)
					return
				}
			}
		}()
		return out, nil
	}
}

// drain reads ch until it is closed, discarding values. It lets a provider
// goroutine that is blocked sending on ch make progress and terminate after the
// consumer has gone away, so neither the provider goroutine nor the pool slot
// leaks on cancellation.
func drain(ch <-chan provider.Chunk) {
	for range ch {
	}
}
