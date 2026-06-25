// Package resilience is the composition root that wires the retry engine
// (COMP-003, internal/proxy/retry) and the per-provider circuit breaker
// (COMP-011, internal/breaker) into the single provider-call seam the server
// exposes (server.InferFunc, ADR-0006).
//
// Composition (ADR-0006): retry( breaker.Execute( providerCall ) ). The breaker
// guards a single provider call keyed by model/provider name; the retry loop
// wraps the breaker call. Because gobreaker.ErrOpenState is registered as
// non-retryable, an open breaker fast-fails through the retry layer without
// burning the retry budget.
//
// Error mapping for the server boundary:
//   - open breaker (ErrOpenState) or a deadline/cancellation during retry →
//     *Unavailable (server → 503 + Retry-After, AC-022/AC-020).
//   - exhausted retries on transient 5xx → wrapped retry.ErrExhausted (server →
//     502, AC-019).
//   - a 4xx provider StatusError → returned unwrapped, not retried (server →
//     502 per the existing provider-error mapping, AC-021).
//
// The seam is kept clean for CARD-008 (worker pool): InferFunc returns the same
// signature, so the worker pool can wrap this composed func without changes.
package resilience

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/adverax/sluice/internal/breaker"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy/retry"
	"github.com/adverax/sluice/internal/server"
)

// Unavailable signals that the gateway fast-failed without (or after abandoning)
// a usable upstream result: the circuit breaker is open, or the client's
// deadline elapsed during a retry. The server maps it to HTTP 503 and surfaces
// the Retry-After hint via the Retry-After header (AC-022, AC-020).
//
// It satisfies errors.Is(err, server.ErrServiceUnavailable) so the server can
// classify it without importing this package (no import cycle), and exposes
// RetryAfter() so the server can set the header.
type Unavailable struct {
	// Reason is a short, machine-stable cause ("breaker_open" | "deadline").
	Reason string
	// retryAfter is the hint for the Retry-After header (seconds granularity).
	retryAfter time.Duration
	// Err is the wrapped cause (ErrOpenState, context error).
	Err error
}

// Error implements error.
func (u *Unavailable) Error() string {
	return fmt.Sprintf("resilience: service unavailable (%s): %v", u.Reason, u.Err)
}

// Unwrap exposes the wrapped cause for errors.Is/As against ErrOpenState and
// context errors.
func (u *Unavailable) Unwrap() error { return u.Err }

// Is reports a match against server.ErrServiceUnavailable so the server maps any
// Unavailable to HTTP 503 via errors.Is, without importing this package.
func (u *Unavailable) Is(target error) bool {
	return target == server.ErrServiceUnavailable
}

// RetryAfter returns the Retry-After hint for the 503 response header (AC-022).
func (u *Unavailable) RetryAfter() time.Duration { return u.retryAfter }

// Composer builds the composed InferFunc. It holds the retry engine and the
// per-provider breaker registry, both injected (ADR-0008).
type Composer struct {
	retrier    *retry.Engine
	breakers   *breaker.Registry
	retryAfter time.Duration
}

// New builds a Composer. retryAfter is the Retry-After hint surfaced on a 503
// fast-fail (typically config.Breaker.RetryAfter).
func New(retrier *retry.Engine, breakers *breaker.Registry, retryAfter time.Duration) *Composer {
	return &Composer{retrier: retrier, breakers: breakers, retryAfter: retryAfter}
}

// IsOpenState reports whether err is the open-breaker signal. The composition
// root passes this to retry.WithNonRetryable so the retry layer never spins
// against an open breaker (ADR-0006).
func IsOpenState(err error) bool {
	return errors.Is(err, breaker.ErrOpenState)
}

// InferFunc returns the composed server.InferFunc: retry(breaker.Execute(call)).
// The breaker is keyed by the request model (the per-provider key, FR-007 — in
// v1 one model maps to one provider). On ErrOpenState or a retry deadline it
// returns an *Unavailable so the server emits 503 + Retry-After.
func (c *Composer) InferFunc() server.InferFunc {
	return func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
		key := req.Model

		// The innermost call: a single provider inference, guarded by the breaker.
		guarded := func(ctx context.Context) (provider.Response, error) {
			return c.breakers.Execute(ctx, key, func(ctx context.Context) (provider.Response, error) {
				return p.Infer(ctx, req)
			})
		}

		resp, err := c.retrier.Do(ctx, guarded)
		if err == nil {
			return resp, nil
		}

		// Open breaker → fast-fail 503 (AC-022). The retry layer propagated it
		// unwrapped because IsOpenState marked it non-retryable.
		if IsOpenState(err) {
			return provider.Response{}, &Unavailable{
				Reason:     "breaker_open",
				retryAfter: c.retryAfter,
				Err:        err,
			}
		}

		// Deadline/cancellation (during the call or a retry backoff) → 503 with
		// cancellation information (AC-020).
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return provider.Response{}, &Unavailable{
				Reason:     "deadline",
				retryAfter: c.retryAfter,
				Err:        err,
			}
		}

		// Exhausted retries on a transient failure, or a non-retryable 4xx:
		// propagate so the server maps it to 502 (AC-019/AC-021).
		return provider.Response{}, err
	}
}

// StreamFunc returns the composed server.StreamFunc for the streaming path
// (CARD-014): breaker.ExecuteStream(provider.InferStream), with NO retry — a
// partially-sent stream cannot be safely replayed, so the retry engine is
// deliberately absent from this seam (ADR-0006, documented choice).
//
// The breaker guards stream INITIATION keyed by the request model (the same
// per-provider key/registry as the unary path, FR-007): an OPEN breaker returns
// ErrOpenState immediately (mapped here to *Unavailable → server 503 + Retry-
// After, BEFORE any SSE byte, AC-014a); a successful initiation counts as a
// breaker success; an initiation error counts as a breaker failure. Mid-stream
// chunk errors do NOT feed the breaker in v1 (see breaker.ExecuteStream).
//
// In cmd/gateway the worker pool wraps this composed func (pool.GuardStream) so
// the layering is pool → breaker → provider.InferStream, mirroring the unary
// pool → retry → breaker → provider.
func (c *Composer) StreamFunc() server.StreamFunc {
	return func(ctx context.Context, p provider.Provider, req provider.Request) (<-chan provider.Chunk, error) {
		key := req.Model

		ch, err := c.breakers.ExecuteStream(ctx, key, func(ctx context.Context) (<-chan provider.Chunk, error) {
			return p.InferStream(ctx, req)
		})
		if err == nil {
			return ch, nil
		}

		// Open breaker → fast-fail 503 (AC-014a). The server resolves this BEFORE
		// writing the SSE 200 header, so the client gets a real 503.
		if IsOpenState(err) {
			return nil, &Unavailable{
				Reason:     "breaker_open",
				retryAfter: c.retryAfter,
				Err:        err,
			}
		}

		// Client cancellation/deadline at initiation → 503 with cancellation info,
		// consistent with the unary path (AC-020).
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, &Unavailable{
				Reason:     "deadline",
				retryAfter: c.retryAfter,
				Err:        err,
			}
		}

		// Any other initiation failure (provider/transport error): propagate so the
		// server maps it to 502 (mirrors the unary provider-error mapping).
		return nil, err
	}
}
