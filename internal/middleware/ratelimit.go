// Package middleware holds the net/http middleware that runs OUTSIDE the
// generated route boundary. The rate-limit middleware (COMP-008, FR-004) is the
// first cross-cutting concern in the chain (ADR-0006): it reads the API key from
// the Authorization header, enforces the per-key limit, and returns 429 BEFORE
// any provider/proxy/pool work is started (INV-004).
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adverax/sluice/internal/ratelimit"
)

const (
	// apiKeyHeader is the response header carrying a minted ephemeral key
	// (ADR-0001 / AC-012).
	apiKeyHeader = "X-Sluice-Api-Key"
	// apiKeyCookie is the cookie name carrying the minted ephemeral key so
	// browser clients reuse it automatically (ADR-0001).
	apiKeyCookie = "sluice_api_key"
	// ephemeralKeyPrefix namespaces minted keys so they are distinguishable from
	// client-supplied keys in logs/metrics.
	ephemeralKeyPrefix = "eph_"
	// defaultRetryAfter is the floor for the Retry-After header when a more
	// precise hint is unavailable.
	defaultRetryAfter = time.Second
)

// localLimiter is the local per-key token-bucket surface the middleware needs.
// *ratelimit.Registry satisfies it; narrowing keeps the middleware testable.
type localLimiter interface {
	Allow(key string) ratelimit.Decision
}

// RateLimiter is the net/http rate-limit middleware (COMP-008). It composes the
// LOCAL token-bucket registry (fast path) with the DISTRIBUTED repository (the
// global cross-instance cap, ADR-0010). A request must pass BOTH tiers to reach
// the next handler.
//
// Fail-open policy (resilience, DOCUMENTED): if the distributed repository
// returns an ERROR (e.g. Redis is down), the middleware FAILS OPEN — it falls
// back to the local-limiter verdict rather than rejecting the request. A Redis
// blip therefore does not turn into a fleet-wide 429/503; the local limiter
// still bounds per-instance burst. The alternative (fail-closed) would amplify a
// dependency outage into a total outage, which is the worse failure mode for a
// proxy whose job is availability. This choice is recorded in the card Worktree
// notes and ADR-0010's spirit (graceful degradation).
type RateLimiter struct {
	local     localLimiter
	repo      ratelimit.RateLimitRepository
	globalRPS int
	window    time.Duration
	logger    *slog.Logger
	cookie    bool
	mintKey   func() string
}

// RateLimiterOption configures a RateLimiter (functional options, CON-001).
type RateLimiterOption func(*RateLimiter)

// WithCookie toggles emitting the minted ephemeral key as a Set-Cookie header
// (in addition to the X-Sluice-Api-Key response header). Default: on.
func WithCookie(enabled bool) RateLimiterOption {
	return func(m *RateLimiter) { m.cookie = enabled }
}

// WithKeyMinter overrides the ephemeral-key generator (test seam). The default
// uses crypto/rand (security: never math/rand for keys).
func WithKeyMinter(fn func() string) RateLimiterOption {
	return func(m *RateLimiter) {
		if fn != nil {
			m.mintKey = fn
		}
	}
}

// NewRateLimiter builds the middleware. local is the per-key token-bucket
// registry; repo is the distributed global-cap port (pass an in-memory or Redis
// implementation, or nil to disable the distributed tier and rely on the local
// limiter alone). globalRPS/window define the distributed cap. The logger is
// injected (ADR-0008).
func NewRateLimiter(local localLimiter, repo ratelimit.RateLimitRepository, globalRPS int, window time.Duration, logger *slog.Logger, opts ...RateLimiterOption) *RateLimiter {
	m := &RateLimiter{
		local:     local,
		repo:      repo,
		globalRPS: globalRPS,
		window:    window,
		logger:    logger,
		cookie:    true,
		mintKey:   mintEphemeralKey,
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.window <= 0 {
		m.window = time.Second
	}
	return m
}

// Middleware returns the http middleware function (the chain link).
func (m *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, minted := m.resolveKey(r)

		// On a minted key, advertise it to the client BEFORE we might reject, so
		// even a 429 carries the key the client should reuse (ADR-0001 / AC-012).
		if minted {
			w.Header().Set(apiKeyHeader, key)
			if m.cookie {
				http.SetCookie(w, &http.Cookie{
					Name:     apiKeyCookie,
					Value:    key,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
				})
			}
		}

		// Tier 1: LOCAL token bucket (fast path, no network).
		if d := m.local.Allow(key); !d.Allowed {
			m.reject(w, r, d.RetryAfter, "local")
			return
		}

		// Tier 2: DISTRIBUTED global cap (cross-instance, ADR-0010). Fail open on
		// a backend error so a Redis outage degrades to local-only limiting.
		if m.repo != nil {
			d, err := m.repo.Allow(r.Context(), key, m.globalRPS, m.window)
			if err != nil {
				m.logger.LogAttrs(r.Context(), slog.LevelWarn,
					"rate-limit distributed check failed; failing open to local limiter",
					slog.String("error", err.Error()),
				)
			} else if !d.Allowed {
				m.reject(w, r, d.RetryAfter, "distributed")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// resolveKey extracts the API key from the Authorization header. When absent it
// mints a crypto-random ephemeral key (ADR-0001) and reports minted=true so the
// caller advertises it on the response.
func (m *RateLimiter) resolveKey(r *http.Request) (key string, minted bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return m.mintKey(), true
	}
	// Accept "Bearer <token>" and raw token forms; the token is an identifier
	// only — it is NOT validated against any store (ADR-0001 non-goal).
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		auth = strings.TrimSpace(after)
	}
	if auth == "" {
		return m.mintKey(), true
	}
	return auth, false
}

// reject writes a 429 with a Retry-After header and does NOT call next, so no
// provider/proxy/pool work runs (AC-010, INV-004).
func (m *RateLimiter) reject(w http.ResponseWriter, r *http.Request, retryAfter time.Duration, tier string) {
	secs := int(retryAfter.Round(time.Second) / time.Second)
	if retryAfter <= 0 {
		secs = int(defaultRetryAfter / time.Second)
	}
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"rate_limited","message":"rate limit exceeded; retry later"}`))

	m.logger.LogAttrs(r.Context(), slog.LevelInfo, "request rate-limited",
		slog.String("tier", tier),
		slog.Int("retry_after_s", secs),
	)
}

// mintEphemeralKey returns a cryptographically random ephemeral key (ADR-0001,
// security: crypto/rand, never math/rand). On the practically-impossible RNG
// failure it falls back to a timestamp-derived value so a request is never left
// without a key.
func mintEphemeralKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ephemeralKeyPrefix + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return ephemeralKeyPrefix + hex.EncodeToString(b[:])
}
