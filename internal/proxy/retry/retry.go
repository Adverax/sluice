// Package retry implements COMP-003, the retry engine (FR-006). It wraps a
// provider call with bounded, deadline-aware exponential backoff + jitter and
// retries ONLY on safe/transient failures.
//
// Ports & adapters (forge:engineering-standards): the engine operates on a
// generic Call — `func(ctx) (provider.Response, error)` — not on a concrete
// provider type. The circuit breaker (internal/breaker) wraps the same Call
// shape, so they compose as `retry(breaker.Execute(providerCall))` (ADR-0006)
// without either layer knowing about the other's internals.
//
// Classification of retryable vs non-retryable is done via typed/sentinel
// errors, never by string-matching (engineering standards): a *provider.
// StatusError with a 5xx code is retryable; a 4xx is not (AC-021); a
// context-cancellation error is not retryable (AC-020); and a caller-supplied
// "stop" predicate (used to mark gobreaker.ErrOpenState non-retryable, ADR-0006)
// short-circuits the loop immediately.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/provider"
)

// Call is the unit of work the engine retries: a single attempt against a
// resolved provider. It MUST honour ctx. This is the same shape the breaker
// wraps, which is what lets the two compose (ADR-0006).
type Call func(ctx context.Context) (provider.Response, error)

// ErrExhausted wraps the last error after all attempts are spent. The server
// maps it to HTTP 502 (AC-019). Match the underlying cause with errors.Is on
// the wrapped error; match exhaustion itself with errors.Is(err, ErrExhausted).
var ErrExhausted = errors.New("retry: attempts exhausted")

// retryable is satisfied by errors that can classify themselves (e.g.
// *provider.StatusError). It lets the engine ask the error directly instead of
// string-matching.
type retryable interface {
	Retryable() bool
}

// Engine performs bounded, deadline-aware retries with exponential backoff and
// jitter (FR-006). It is stateless beyond its configuration and safe for
// concurrent use. Construct it with New.
type Engine struct {
	cfg config.Retry

	// nonRetryable, when non-nil, reports whether an error must NOT be retried
	// regardless of its own classification. The composition root sets this to
	// treat gobreaker.ErrOpenState as a fast-fail signal (ADR-0006) so the retry
	// layer never spins against an open breaker.
	nonRetryable func(error) bool

	// sleep waits for d honouring ctx; it is injectable so tests run without
	// real delays. It returns ctx.Err() if ctx is cancelled before d elapses.
	sleep func(ctx context.Context, d time.Duration) error

	// rand returns a float in [0,1); injectable for deterministic jitter in
	// tests.
	rand func() float64
}

// Option configures an Engine (functional options, CON-001).
type Option func(*Engine)

// WithNonRetryable installs a predicate marking certain errors as
// non-retryable regardless of their own classification. Used by the composition
// root to short-circuit on gobreaker.ErrOpenState (ADR-0006).
func WithNonRetryable(fn func(error) bool) Option {
	return func(e *Engine) {
		if fn != nil {
			e.nonRetryable = fn
		}
	}
}

// WithSleep overrides the backoff sleep (tests inject a no-op or recorder to
// avoid real delays).
func WithSleep(fn func(ctx context.Context, d time.Duration) error) Option {
	return func(e *Engine) {
		if fn != nil {
			e.sleep = fn
		}
	}
}

// WithRand overrides the jitter source for deterministic tests.
func WithRand(fn func() float64) Option {
	return func(e *Engine) {
		if fn != nil {
			e.rand = fn
		}
	}
}

// New builds an Engine from the retry configuration.
func New(cfg config.Retry, opts ...Option) *Engine {
	e := &Engine{
		cfg:   cfg,
		sleep: sleepCtx,
		rand:  rand.Float64,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Do runs call with bounded retries. Semantics:
//
//   - Before EVERY attempt (including the first) it checks ctx.Err(); if the
//     deadline is exceeded or ctx is cancelled it returns promptly with the
//     cancellation error and does NOT start the attempt (AC-020).
//   - On success it returns the response.
//   - On a non-retryable error (4xx StatusError, a context error, or one matched
//     by the nonRetryable predicate such as gobreaker.ErrOpenState) it returns
//     that error immediately, unwrapped, so the caller can classify it
//     (ADR-0006: ErrOpenState → 503, 4xx → passthrough).
//   - On a retryable error it backs off (exponential + jitter, capped, ctx-aware)
//     and tries again until MaxAttempts is reached, then returns the last error
//     wrapped in ErrExhausted (AC-019 → 502).
func (e *Engine) Do(ctx context.Context, call Call) (provider.Response, error) {
	attempts := e.cfg.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Deadline-aware: never start an attempt against a cancelled/expired ctx
		// (AC-020). This also covers the gap during backoff.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return provider.Response{}, fmt.Errorf("retry: aborted after %d attempt(s): %w", attempt-1, err)
			}
			return provider.Response{}, err
		}

		resp, err := call(ctx)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Non-retryable classification — return immediately, unwrapped, so the
		// caller sees the original error (ErrOpenState / 4xx / ctx).
		if !e.shouldRetry(ctx, err) {
			return provider.Response{}, err
		}

		// Last attempt failed with a retryable error → exhausted (AC-019).
		if attempt == attempts {
			break
		}

		// Back off before the next attempt, honouring ctx (AC-020): if the
		// deadline elapses during the wait we abort instead of retrying.
		if serr := e.sleep(ctx, e.backoff(attempt)); serr != nil {
			return provider.Response{}, fmt.Errorf("retry: aborted during backoff after %d attempt(s): %w", attempt, serr)
		}
	}

	return provider.Response{}, fmt.Errorf("%w after %d attempt(s): %w", ErrExhausted, attempts, lastErr)
}

// shouldRetry classifies err. Order matters: the caller-supplied non-retryable
// predicate (ErrOpenState) and context errors win over status classification.
func (e *Engine) shouldRetry(ctx context.Context, err error) bool {
	if e.nonRetryable != nil && e.nonRetryable(err) {
		return false
	}
	// Context cancellation/deadline is never retryable (AC-020).
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	// Typed classification (AC-021): a StatusError decides for itself; a 4xx is
	// not retryable, a 5xx is.
	var re retryable
	if errors.As(err, &re) {
		return re.Retryable()
	}
	// Unknown errors are treated as transient network-style failures and are
	// retried — a bare provider failure with no status is assumed transient.
	return true
}

// backoff computes the delay before the retry following the given (1-based)
// attempt number: BaseDelay * 2^(attempt-1), capped at MaxDelay, with up to
// Jitter fraction of randomised reduction applied to spread retries.
func (e *Engine) backoff(attempt int) time.Duration {
	base := e.cfg.BaseDelay
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= e.cfg.MaxDelay {
			d = e.cfg.MaxDelay
			break
		}
	}
	if e.cfg.MaxDelay > 0 && d > e.cfg.MaxDelay {
		d = e.cfg.MaxDelay
	}
	if e.cfg.Jitter > 0 {
		// Subtract up to Jitter fraction at random: delay in
		// [d*(1-Jitter), d]. Keeps retries bounded by MaxDelay.
		jitter := float64(d) * e.cfg.Jitter * e.rand()
		d -= time.Duration(jitter)
	}
	if d < 0 {
		d = 0
	}
	return d
}

// sleepCtx waits for d honouring ctx, returning ctx.Err() if ctx is cancelled
// first. A non-positive d still observes an already-cancelled ctx.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
