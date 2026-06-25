// Package ratelimit implements per-API-key rate limiting (COMP-008 / COMP-009,
// FR-004) per ADR-0001 (ephemeral_assigned_key) and ADR-0010 (per-context
// repository interface).
//
// Two tiers cooperate:
//
//   - A LOCAL token-bucket registry (golang.org/x/time/rate, one limiter per
//     key) is the fast in-process path. It bounds a single instance's burst per
//     key with no network round-trip on the hot path.
//   - A DISTRIBUTED RateLimitRepository (the port) enforces a GLOBAL cap shared
//     across gateway instances that point at the same backing store (Redis,
//     ADR-0010). The middleware consults the local limiter first, then the
//     repository, so a request must pass BOTH to be served (AC-013).
//
// The middleware depends only on the RateLimitRepository interface (ports &
// adapters): the go-redis adapter lives in redisrepo.go, an in-memory adapter
// (used by the global-cap unit test and as a default) lives in memrepo.go.
//
// Eviction note (deferred, see ADR-0001 "Negative"): the per-key limiter
// registry grows with the number of distinct keys. v1 does NOT evict; a TTL /
// LRU sweep over idle buckets is a documented follow-up. The growth is bounded
// in practice by well-behaved clients reusing their issued ephemeral key.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Decision is the verdict for one rate-limit check against one key.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// RetryAfter is a hint for the Retry-After header when Allowed is false. It
	// is best-effort; callers fall back to a sane default when it is zero.
	RetryAfter time.Duration
}

// RateLimitRepository is the port (ADR-0010) for the DISTRIBUTED, cross-instance
// rate limit. Implementations enforce a global cap of `limit` events per
// `window` for the given key, atomically, so that N gateway instances sharing
// one backend never collectively exceed the cap (AC-013).
//
// Implementations MUST be safe for concurrent use. A non-nil error signals that
// the backend itself failed (e.g. Redis is unreachable); the middleware treats
// such an error as fail-open (see Middleware docs) rather than failing the
// request, so a backend blip does not 503 all traffic.
type RateLimitRepository interface {
	// Allow atomically records one event for key against a global cap of `limit`
	// per `window` and reports whether the event is within the cap. On a backend
	// failure it returns a non-nil error and the Decision is ignored.
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error)
}

// Registry is the per-key LOCAL token-bucket registry. Each distinct key gets
// its own *rate.Limiter sized from the configured rps/burst. It is safe for
// concurrent use.
//
// A clock function is injected (defaults to time.Now) so tests can drive the
// token buckets deterministically via rate.Limiter's reservation API without
// real-time sleeps.
type Registry struct {
	rps   rate.Limit
	burst int
	now   func() time.Time

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

// Option configures a Registry (functional options, CON-001).
type Option func(*Registry)

// WithClock injects the clock used by every limiter the registry creates. It is
// primarily a test seam for deterministic token accounting.
func WithClock(now func() time.Time) Option {
	return func(r *Registry) {
		if now != nil {
			r.now = now
		}
	}
}

// NewRegistry builds a per-key limiter registry. rps is the steady refill rate
// and burst is the bucket capacity; both must be > 0 (config validation
// guarantees this — NewRegistry panics otherwise, fail-loud at boot).
func NewRegistry(rps, burst int, opts ...Option) *Registry {
	if rps <= 0 {
		panic("ratelimit: rps must be > 0")
	}
	if burst <= 0 {
		panic("ratelimit: burst must be > 0")
	}
	r := &Registry{
		rps:      rate.Limit(rps),
		burst:    burst,
		now:      time.Now,
		limiters: make(map[string]*rate.Limiter),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// limiterFor returns the limiter for key, creating a fresh bucket on first use.
func (r *Registry) limiterFor(key string) *rate.Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	lim, ok := r.limiters[key]
	if !ok {
		lim = rate.NewLimiter(r.rps, r.burst)
		r.limiters[key] = lim
	}
	return lim
}

// Allow reports whether one request under key is permitted by the LOCAL token
// bucket. It consumes a token when allowed. When denied it returns a RetryAfter
// hint derived from the bucket's reservation delay.
func (r *Registry) Allow(key string) Decision {
	lim := r.limiterFor(key)
	now := r.now()
	res := lim.ReserveN(now, 1)
	if !res.OK() {
		// Burst smaller than the requested tokens — should not happen with N=1
		// and burst>=1, but treat as denied defensively.
		return Decision{Allowed: false, RetryAfter: r.window()}
	}
	delay := res.DelayFrom(now)
	if delay > 0 {
		// Not enough tokens right now: cancel the reservation (do not consume a
		// future token) and deny with the wait as the Retry-After hint.
		res.CancelAt(now)
		return Decision{Allowed: false, RetryAfter: delay}
	}
	return Decision{Allowed: true}
}

// window returns the refill period of one token (1/rps), used as a Retry-After
// fallback when a reservation is rejected outright.
func (r *Registry) window() time.Duration {
	if r.rps <= 0 {
		return time.Second
	}
	return time.Duration(float64(time.Second) / float64(r.rps))
}
