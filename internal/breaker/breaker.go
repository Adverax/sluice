// Package breaker implements COMP-011, the per-provider circuit breaker
// (FR-007), on top of github.com/sony/gobreaker tuned per ADR-0002
// (volume_based_50pct).
//
// Ports & adapters (forge:engineering-standards): a breaker wraps a generic
// Call — `func(ctx) (provider.Response, error)` — keyed by provider/model name,
// not a concrete provider type. This is the same Call shape the retry engine
// (internal/proxy/retry) consumes, so they compose as
// `retry(breaker.Execute(providerCall))` (ADR-0006).
//
// Open-state semantics (INV-005, AC-022): when the breaker is open,
// Execute returns gobreaker.ErrOpenState IMMEDIATELY without invoking the
// underlying call (fast-fail, latency < 1ms). The composition root treats
// ErrOpenState as non-retryable (ADR-0006) and the server maps it to 503 +
// Retry-After.
package breaker

import (
	"context"
	"errors"
	"sync"

	"github.com/sony/gobreaker"

	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/provider"
)

// ErrOpenState is re-exported so callers can match the open-breaker condition
// (for the non-retryable predicate and the 503 mapping) without importing
// gobreaker directly.
var ErrOpenState = gobreaker.ErrOpenState

// Call is the unit of work a breaker guards: a single attempt against a
// resolved provider. It MUST honour ctx. Identical to retry.Call so the two
// layers compose (ADR-0006).
type Call func(ctx context.Context) (provider.Response, error)

// Registry holds one gobreaker.CircuitBreaker per provider/model key and builds
// new ones lazily on first use (FR-007: per-provider breaker). The Router knows
// the provider names; the composition root keys the breaker by model/provider
// name. Safe for concurrent use.
type Registry struct {
	cfg config.Breaker

	// onStateChange is forwarded to gobreaker so the composition root can log
	// transitions / emit EVT-004 (AC-023). Optional.
	onStateChange func(name string, from, to gobreaker.State)

	// settingsFor builds the gobreaker.Settings for a key; injectable so tests
	// can override timing (e.g. a short Timeout for the half-open test, AC-024)
	// without waiting 60s.
	settingsFor func(name string) gobreaker.Settings

	mu       sync.Mutex
	breakers map[string]*gobreaker.CircuitBreaker
}

// Option configures a Registry (functional options, CON-001).
type Option func(*Registry)

// WithOnStateChange installs a callback invoked on every breaker state
// transition (used to log / emit EVT-004 on open, AC-023).
func WithOnStateChange(fn func(name string, from, to gobreaker.State)) Option {
	return func(r *Registry) { r.onStateChange = fn }
}

// WithSettings overrides how a breaker's gobreaker.Settings are built for a
// given key. Tests use it to inject a short Timeout so the half-open transition
// is observable without a real 60s wait (AC-024). Production wiring leaves the
// ADR-0002 defaults from config in place.
func WithSettings(fn func(name string) gobreaker.Settings) Option {
	return func(r *Registry) {
		if fn != nil {
			r.settingsFor = fn
		}
	}
}

// NewRegistry builds a Registry from the breaker configuration.
func NewRegistry(cfg config.Breaker, opts ...Option) *Registry {
	r := &Registry{
		cfg:      cfg,
		breakers: make(map[string]*gobreaker.CircuitBreaker),
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.settingsFor == nil {
		r.settingsFor = r.defaultSettings
	}
	return r
}

// defaultSettings builds the ADR-0002 (volume_based_50pct) gobreaker.Settings:
// tumbling Interval, open→half-open Timeout, MaxRequests probes in half-open,
// and a volume-gated 50% failure-ratio ReadyToTrip.
func (r *Registry) defaultSettings(name string) gobreaker.Settings {
	minReq := r.cfg.MinRequests
	ratio := r.cfg.FailureRatio
	return gobreaker.Settings{
		Name:        name,
		Interval:    r.cfg.Interval,
		Timeout:     r.cfg.Timeout,
		MaxRequests: r.cfg.MaxRequests,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests < minReq {
				return false
			}
			return float64(counts.TotalFailures)/float64(counts.Requests) >= ratio
		},
		OnStateChange: func(n string, from, to gobreaker.State) {
			if r.onStateChange != nil {
				r.onStateChange(n, from, to)
			}
		},
	}
}

// get returns the breaker for key, creating it on first use.
func (r *Registry) get(key string) *gobreaker.CircuitBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cb, ok := r.breakers[key]; ok {
		return cb
	}
	cb := gobreaker.NewCircuitBreaker(r.settingsFor(key))
	r.breakers[key] = cb
	return cb
}

// State reports the current state of the breaker for key (creating it if it
// does not yet exist). Useful for tests and observability.
func (r *Registry) State(key string) gobreaker.State {
	return r.get(key).State()
}

// Execute runs call through the breaker keyed by key. In the open state it
// returns ErrOpenState immediately without invoking call (fast-fail, AC-022).
//
// Client-originated cancellation / deadline (context.Canceled,
// context.DeadlineExceeded) is NOT counted as a provider failure: a client
// hanging up or timing out is not the provider's fault and must not trip a
// healthy provider's breaker. We achieve this by wrapping such errors in a
// sentinel that gobreaker's IsSuccessful hook treats as a success, then
// unwrapping them after Execute returns so the original ctx error propagates
// to the caller unchanged.
func (r *Registry) Execute(ctx context.Context, key string, call Call) (provider.Response, error) {
	cb := r.get(key)

	// ctxErr wraps a context error so gobreaker counts the call as a non-failure.
	// It carries the original error so we can unwrap it after Execute.
	type ctxErr struct{ cause error }
	// ctxErrSentinel is returned from the inner func to signal "treat as success
	// for accounting purposes, but propagate the real error to the caller".
	// We implement this by setting IsSuccessful on the Settings (see below), but
	// gobreaker does not allow per-Execute overrides; the simpler approach is to
	// store the ctx error in the interface{} result and NOT return an error so
	// gobreaker counts it as a success, then detect it on the res side.
	res, err := cb.Execute(func() (interface{}, error) {
		resp, callErr := call(ctx)
		if callErr != nil && (errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded)) {
			// Return the ctx error inside the result (not as the error return)
			// so gobreaker records this as a successful call and does NOT
			// increment its failure counter.
			return ctxErr{cause: callErr}, nil
		}
		return resp, callErr
	})
	if err != nil {
		// res may be nil (ErrOpenState / ErrTooManyRequests) or a provider.Response
		// when gobreaker's half-open probe itself observed an error — use comma-ok.
		if resp, ok := res.(provider.Response); ok {
			return resp, err
		}
		return provider.Response{}, err
	}
	// Unwrap a context-originated error that was hidden from gobreaker's counter.
	if ce, ok := res.(ctxErr); ok {
		return provider.Response{}, ce.cause
	}
	// Happy path: res must be a provider.Response — use comma-ok for safety.
	if resp, ok := res.(provider.Response); ok {
		return resp, nil
	}
	return provider.Response{}, nil
}
