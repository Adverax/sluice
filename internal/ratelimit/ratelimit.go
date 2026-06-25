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
// Eviction: the registry is bounded by maxKeys (GATEWAY_RATELIMIT_MAX_KEYS,
// default 100 000). A background sweep runs every sweepInterval and evicts
// limiters whose token bucket is FULL (tokens == burst) — meaning the key is
// idle and not actively limited, so dropping it is safe. When the registry
// would exceed maxKeys a hard-cap eviction removes the least-recently-used
// entry immediately. Both paths are mutex-protected and the sweep goroutine
// is stopped via Close / a context (FR-012 graceful shutdown).
package ratelimit

import (
	"context"
	"sort"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// defaultMaxKeys is the hard cap on the number of per-key limiters the
// registry will hold. Configurable via GATEWAY_RATELIMIT_MAX_KEYS.
const defaultMaxKeys = 100_000

// defaultSweepInterval is how often the background goroutine sweeps for
// full-bucket (idle) entries to evict.
const defaultSweepInterval = 5 * time.Minute

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

// entry is a registry slot that pairs a limiter with the last-access time
// (used for LRU-style eviction when the registry is over cap).
type entry struct {
	lim        *rate.Limiter
	lastAccess time.Time
}

// Registry is the per-key LOCAL token-bucket registry. Each distinct key gets
// its own *rate.Limiter sized from the configured rps/burst. It is safe for
// concurrent use.
//
// The registry is bounded: it will not grow beyond maxKeys entries. Idle
// limiters (full token bucket) are swept periodically; the hard cap evicts the
// least-recently-used entry on overflow. Close stops the background sweep
// goroutine (required for graceful shutdown — FR-012).
//
// A clock function is injected (defaults to time.Now) so tests can drive the
// token buckets deterministically via rate.Limiter's reservation API without
// real-time sleeps.
type Registry struct {
	rps           rate.Limit
	burst         int
	maxKeys       int
	sweepInterval time.Duration
	now           func() time.Time

	mu       sync.Mutex
	limiters map[string]*entry

	stopSweep context.CancelFunc
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

// WithMaxKeys sets the hard cap on the number of per-key limiters the registry
// will hold. When the registry is at cap a new key evicts the least-recently-
// used entry. Defaults to defaultMaxKeys (100 000). Must be > 0.
func WithMaxKeys(n int) Option {
	return func(r *Registry) {
		if n > 0 {
			r.maxKeys = n
		}
	}
}

// WithSweepInterval overrides the idle-sweep period (test seam). Must be > 0.
func WithSweepInterval(d time.Duration) Option {
	return func(r *Registry) {
		if d > 0 {
			r.sweepInterval = d
		}
	}
}

// NewRegistry builds a per-key limiter registry. rps is the steady refill rate
// and burst is the bucket capacity; both must be > 0 (config validation
// guarantees this — NewRegistry panics otherwise, fail-loud at boot).
// The registry starts a background sweep goroutine; call Close when done.
func NewRegistry(rps, burst int, opts ...Option) *Registry {
	if rps <= 0 {
		panic("ratelimit: rps must be > 0")
	}
	if burst <= 0 {
		panic("ratelimit: burst must be > 0")
	}
	r := &Registry{
		rps:           rate.Limit(rps),
		burst:         burst,
		maxKeys:       defaultMaxKeys,
		sweepInterval: defaultSweepInterval,
		now:           time.Now,
		limiters:      make(map[string]*entry),
	}
	for _, opt := range opts {
		opt(r)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.stopSweep = cancel
	go r.sweepLoop(ctx)

	return r
}

// Close stops the background sweep goroutine. It is safe to call multiple
// times and is idempotent. Should be called during graceful shutdown (FR-012).
func (r *Registry) Close() {
	r.stopSweep()
}

// sweepLoop periodically evicts limiters whose token bucket is fully
// replenished (idle). A full bucket means the key is not actively limited;
// dropping it is safe and reclaims memory.
func (r *Registry) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(r.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweepIdle()
		}
	}
}

// sweepIdle removes all entries whose limiter has a full token bucket at the
// current clock time. Full bucket ⇒ no tokens have been consumed recently ⇒
// key is idle ⇒ safe to evict.
func (r *Registry) sweepIdle() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.limiters {
		// TokensAt reports the number of tokens available at time t. If it equals
		// the burst capacity the bucket is full ⇒ the key is idle.
		if e.lim.TokensAt(now) >= float64(r.burst) {
			delete(r.limiters, k)
		}
	}
}

// evictLRULocked removes the single entry with the oldest lastAccess time.
// Must be called with r.mu held.
func (r *Registry) evictLRULocked() {
	// Linear scan is acceptable: eviction is rare (only at cap) and the map is
	// capped at maxKeys so the worst-case work is bounded.
	var oldest string
	var oldestTime time.Time
	first := true
	for k, e := range r.limiters {
		if first || e.lastAccess.Before(oldestTime) {
			oldest = k
			oldestTime = e.lastAccess
			first = false
		}
	}
	if oldest != "" {
		delete(r.limiters, oldest)
	}
}

// limiterFor returns the limiter for key, creating a fresh bucket on first use.
// It enforces the maxKeys cap by evicting the LRU entry when the registry is
// full and a new key would be inserted.
func (r *Registry) limiterFor(key string) *rate.Limiter {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.limiters[key]
	if ok {
		e.lastAccess = now
		return e.lim
	}
	// New key: enforce the hard cap before inserting.
	if len(r.limiters) >= r.maxKeys {
		r.evictLRULocked()
	}
	lim := rate.NewLimiter(r.rps, r.burst)
	r.limiters[key] = &entry{lim: lim, lastAccess: now}
	return lim
}

// Len returns the current number of tracked keys. It is primarily a test/
// observability helper.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.limiters)
}

// sortedKeys returns a snapshot of the registry keys sorted by lastAccess
// (ascending). Used only in tests to assert eviction order deterministically.
func (r *Registry) sortedKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	type kv struct {
		key        string
		lastAccess time.Time
	}
	pairs := make([]kv, 0, len(r.limiters))
	for k, e := range r.limiters {
		pairs = append(pairs, kv{k, e.lastAccess})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].lastAccess.Before(pairs[j].lastAccess)
	})
	keys := make([]string, len(pairs))
	for i, p := range pairs {
		keys[i] = p.key
	}
	return keys
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
