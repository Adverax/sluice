// Package config loads and validates the gateway service configuration from
// the environment. All values have defaults so the service is bootable without
// any environment variables set. The GATEWAY_ prefix matches ADR-0003 and the
// developer Makefile.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Defaults for every configurable value. Keeping them in one place makes the
// "all values have defaults" contract (CARD-001) easy to audit.
const (
	defaultAddr = ":8080"

	defaultReadTimeout  = 5 * time.Second
	defaultWriteTimeout = 10 * time.Second
	defaultIdleTimeout  = 120 * time.Second

	defaultShutdownTimeout = 30 * time.Second

	defaultUpstreamTimeout = 30 * time.Second

	// defaultMockUpstreamAddr is the listen address for the IN-PROCESS mock LLM
	// upstream started when GATEWAY_UPSTREAM_URL is empty (CARD-013). It binds to
	// loopback on an ephemeral port so it never collides with the main server and
	// is never exposed off-host; the HTTPProvider is pointed at the resolved
	// address. Override via GATEWAY_MOCK_UPSTREAM_ADDR.
	defaultMockUpstreamAddr = "127.0.0.1:0"

	defaultRedisDialTimeout = 5 * time.Second
	defaultRedisReadTimeout = 3 * time.Second

	defaultPostgresAcquireTimeout = 5 * time.Second

	// defaultHealthCheckTimeout is the per-check deadline passed to each
	// individual readiness checker. It is set to the smaller of the two
	// dependency timeouts so neither Redis nor Postgres can exceed their own
	// configured dial timeout regardless of ordering.
	defaultHealthCheckTimeout = 3 * time.Second

	defaultWorkerPoolSize = 100

	// defaultCacheTTL is the response-cache entry lifetime (COMP-004, FR-005)
	// per ADR-0004 (default 5 minutes; overridable per request via the
	// X-Cache-TTL header). Configurable via GATEWAY_CACHE_TTL (fail-loud).
	defaultCacheTTL = 5 * time.Minute

	// defaultCacheMaxBodyBytes caps the request body buffered by the cache
	// middleware for key computation (GATEWAY_CACHE_MAX_BODY_BYTES). Bodies
	// larger than this limit fall through to the handler WITHOUT caching (not a
	// 413 from the cache layer). 1 MiB is a conservative upper bound for a
	// chat-completion JSON payload; raise it if larger requests need caching.
	defaultCacheMaxBodyBytes = 1 << 20 // 1 MiB

	defaultLogLevel  = "info"
	defaultLogFormat = "json"

	// Retry engine (COMP-003, FR-006). MaxAttempts is the TOTAL number of tries
	// (the first call plus retries), so the default of 3 means one initial call
	// and up to two retries. BaseDelay/MaxDelay bound the exponential backoff and
	// Jitter is the fraction (0..1) of the computed delay applied as random
	// jitter to avoid thundering-herd retries.
	defaultRetryMaxAttempts = 3
	defaultRetryBaseDelay   = 50 * time.Millisecond
	defaultRetryMaxDelay    = 2 * time.Second
	defaultRetryJitter      = 0.5

	// Circuit breaker (COMP-011, FR-007) tuned per ADR-0002 (volume_based_50pct).
	defaultBreakerInterval     = 10 * time.Second // tumbling counter reset
	defaultBreakerTimeout      = 60 * time.Second // open → half-open
	defaultBreakerMaxRequests  = 5                // probes allowed in half-open
	defaultBreakerMinRequests  = 10               // min volume before tripping
	defaultBreakerFailureRatio = 0.5              // failure ratio that trips
	defaultBreakerRetryAfter   = 60 * time.Second // Retry-After hint on 503

	// Rate limiting (COMP-008/COMP-009, FR-004) per ADR-0001/ADR-0010. RPS is the
	// per-key token refill rate; Burst is the bucket capacity (max momentary
	// concurrency). Defaults are deliberately modest (10 rps / burst 20) so the
	// gateway is safe out of the box without env tuning.
	defaultRateLimitRPS   = 10
	defaultRateLimitBurst = 20
	// defaultRateLimitWindow is the bucket window for the distributed counter: the
	// global cap is interpreted as RPS over this window (1s) per ADR-0010.
	defaultRateLimitWindow = time.Second
	// defaultRateLimitMaxKeys is the hard cap on distinct API keys held in the
	// local token-bucket registry. Exceeding this limit evicts the LRU entry
	// (memory-exhaustion / DoS defence). 100 000 is generous for a single
	// instance while remaining safe on a default-provisioned host.
	defaultRateLimitMaxKeys = 100_000

	// Async usage metering (COMP-016/COMP-017, FR-014) per ADR-0005/ADR-0007.
	// BufferSize is the Usage Buffer channel capacity (drop-on-full above this);
	// FlushInterval is the worker's periodic flush trigger.
	defaultMeteringBufferSize    = 1000
	defaultMeteringFlushInterval = 5 * time.Second

	// defaultShutdownHookTimeout is the deadline given to EACH post-drain
	// OnShutdown hook (e.g. the metering worker's Close). It is independent of
	// the HTTP drain deadline so a forced drain (server.Shutdown hitting its own
	// deadline) does not consume the budget available to the hooks. 5s is
	// generous enough for a Postgres flush batch while still bounding total
	// shutdown time (NFR-005). Configurable via GATEWAY_SHUTDOWN_HOOK_TIMEOUT
	// (fail-loud).
	defaultShutdownHookTimeout = 5 * time.Second
)

// Server holds the timeouts applied to the inbound *http.Server boundary
// (NFR-004 / AC-045). Every field must be > 0.
type Server struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// ReadTimeout bounds the time to read the whole request, including body.
	ReadTimeout time.Duration
	// WriteTimeout bounds the time to write the response.
	WriteTimeout time.Duration
	// IdleTimeout bounds keep-alive connection idle time.
	IdleTimeout time.Duration
	// ShutdownTimeout bounds graceful drain on SIGINT/SIGTERM (FR-012).
	ShutdownTimeout time.Duration
}

// Upstream holds the outbound provider boundary configuration: the total
// request timeout for the shared http.Client and the mock-upstream wiring
// (CARD-013).
type Upstream struct {
	// Timeout is the total request timeout for the upstream http.Client.
	Timeout time.Duration

	// URL, when set, points the HTTPProvider at an EXTERNAL mock-upstream HTTP
	// endpoint (GATEWAY_UPSTREAM_URL). When empty, cmd/gateway starts an
	// in-process mock upstream (see MockUpstreamAddr) and points the provider at
	// it. Optional; no default.
	URL string

	// MockUpstreamAddr is the listen address for the in-process mock upstream
	// started when URL is empty (GATEWAY_MOCK_UPSTREAM_ADDR). Defaults to
	// 127.0.0.1:0 (loopback, ephemeral port).
	MockUpstreamAddr string
}

// Redis holds the Redis connection configuration and timeouts (NFR-004).
// The client itself is wired in CARD-003.
type Redis struct {
	// URL is the redis:// connection string.
	URL string
	// DialTimeout bounds establishing a connection.
	DialTimeout time.Duration
	// ReadTimeout bounds a single read from a connection.
	ReadTimeout time.Duration
}

// Postgres holds the Postgres pool configuration and timeouts (NFR-004).
// The pgx pool itself is wired in CARD-003.
type Postgres struct {
	// DSN is the postgres connection string.
	DSN string
	// AcquireTimeout bounds acquiring a connection from the pool.
	AcquireTimeout time.Duration
}

// Logging holds the structured-logger configuration (COMP-015, FR-016).
type Logging struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is "json" (production) or "text" (local dev).
	Format string
}

// Retry holds the tuning for the retry engine (COMP-003, FR-006). It is
// consumed by internal/proxy/retry; the values are validated in Validate.
type Retry struct {
	// MaxAttempts is the TOTAL number of tries (initial call + retries). Must
	// be >= 1; 1 disables retrying.
	MaxAttempts int
	// BaseDelay is the backoff for the first retry; subsequent retries grow
	// exponentially up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff.
	MaxDelay time.Duration
	// Jitter is the fraction (0..1) of the computed delay applied as random
	// jitter to spread retries (avoids thundering herd).
	Jitter float64
}

// Breaker holds the per-provider circuit-breaker tuning (COMP-011, FR-007)
// per ADR-0002 (volume_based_50pct). It is consumed by internal/breaker.
type Breaker struct {
	// Interval is the tumbling counter-reset period in the closed state.
	Interval time.Duration
	// Timeout is the open→half-open recovery period.
	Timeout time.Duration
	// MaxRequests is the number of probe requests allowed in half-open.
	MaxRequests uint32
	// MinRequests is the minimum request volume before the breaker may trip.
	MinRequests uint32
	// FailureRatio is the failure ratio (0..1) at/above which the breaker trips.
	FailureRatio float64
	// RetryAfter is the hint surfaced to clients via the Retry-After header on a
	// fast-fail 503 (open breaker / exhausted retries).
	RetryAfter time.Duration
}

// RateLimit holds the per-API-key rate-limit tuning (COMP-008/COMP-009,
// FR-004) per ADR-0001/ADR-0010. RPS/Burst feed the local golang.org/x/time/rate
// token buckets; RPS/Window feed the distributed (Redis) global cap.
type RateLimit struct {
	// RPS is the per-key refill rate (requests per second).
	RPS int
	// Burst is the per-key token-bucket capacity (max momentary burst).
	Burst int
	// Window is the bucket window for the distributed counter. The global cap is
	// RPS requests per Window across all gateway instances sharing one Redis.
	Window time.Duration
	// MaxKeys is the hard cap on the number of per-key limiters held in the
	// local token-bucket registry (GATEWAY_RATELIMIT_MAX_KEYS). When the
	// registry is at cap the least-recently-used entry is evicted before a new
	// key is inserted (memory-exhaustion / DoS defence).
	MaxKeys int
}

// Cache holds the response-cache tuning (COMP-004, FR-005) per ADR-0004. The
// repository (Redis adapter) is wired in cmd/gateway; only the default TTL lives
// here. The per-request X-Cache-TTL header override is handled in the cache
// middleware, not config.
type Cache struct {
	// TTL is the default cache-entry lifetime applied when a request does not
	// carry a valid X-Cache-TTL override. Must be > 0.
	TTL time.Duration
	// MaxBodyBytes is the per-request body-size cap for cache keying. Bodies
	// exceeding this limit fall through to the handler WITHOUT caching (the
	// cache layer never emits 413; the downstream handler/validator owns that
	// decision). Configurable via GATEWAY_CACHE_MAX_BODY_BYTES (fail-loud).
	// Default: 1 MiB.
	MaxBodyBytes int64
}

// Metering holds the async usage-metering tuning (COMP-016/COMP-017, FR-014)
// per ADR-0005/ADR-0007. The buffer + worker + pgx repository are wired in
// cmd/gateway; only the buffer capacity and flush interval are configurable.
type Metering struct {
	// BufferSize is the Usage Buffer channel capacity (ADR-0005: 1000). Events
	// enqueued when the buffer is full are dropped (ADR-0007). Must be > 0.
	BufferSize int
	// FlushInterval is the worker's periodic flush trigger. The other trigger is
	// the batch filling. Must be > 0.
	FlushInterval time.Duration
}

// Shutdown holds lifecycle shutdown tuning parameters.
type Shutdown struct {
	// HookTimeout is the deadline given to EACH post-drain OnShutdown hook
	// (e.g. the metering worker's Close). It runs from a FRESH context
	// (context.Background()) so an expired HTTP-drain deadline cannot starve
	// the hooks. Must be > 0. Configurable via GATEWAY_SHUTDOWN_HOOK_TIMEOUT
	// (fail-loud). Default 5s.
	HookTimeout time.Duration
}

// Config is the fully-resolved service configuration.
type Config struct {
	Server    Server
	Upstream  Upstream
	Redis     Redis
	Postgres  Postgres
	Logging   Logging
	Retry     Retry
	Breaker   Breaker
	RateLimit RateLimit
	Cache     Cache
	Metering  Metering
	Shutdown  Shutdown

	// HealthCheckTimeout is the per-check deadline passed to each individual
	// readiness checker. Keeping it separate from the Redis/Postgres timeouts
	// lets operators tune the probe SLA independently of the dependency
	// connection timeouts (NFR-004).
	HealthCheckTimeout time.Duration

	// WorkerPoolSize is consumed by CARD-008; loaded here for completeness.
	WorkerPoolSize int
}

// Load reads the configuration from the environment, applies defaults for any
// unset value, and validates it. It returns an error if any value is invalid.
// If an env var is SET but malformed or <= 0, Load returns an error immediately
// (NFR-004 fail-loud). Unset env vars silently use the default.
func Load() (*Config, error) {
	var errs []error

	mustDuration := func(key string, fallback time.Duration) time.Duration {
		d, err := getDuration(key, fallback)
		if err != nil {
			errs = append(errs, err)
			return fallback
		}
		return d
	}

	mustFloat := func(key string, fallback float64) float64 {
		f, err := getFloat(key, fallback)
		if err != nil {
			errs = append(errs, err)
			return fallback
		}
		return f
	}

	// mustPositiveInt reads an int and additionally rejects non-positive values
	// (catches wrapping on uint32 cast and invalid counts). Used for fields where
	// <= 0 is always invalid (attempts, requests).
	mustPositiveInt := func(key string, fallback int) int {
		n, err := getInt(key, fallback)
		if err != nil {
			errs = append(errs, err)
			return fallback
		}
		if n <= 0 {
			errs = append(errs, fmt.Errorf("env %s=%d: value must be > 0", key, n))
			return fallback
		}
		return n
	}

	// mustPositiveInt64 is like mustPositiveInt but for int64 fields (e.g.
	// byte-count caps where an int may be too narrow on 32-bit platforms).
	mustPositiveInt64 := func(key string, fallback int64) int64 {
		n, err := getInt64(key, fallback)
		if err != nil {
			errs = append(errs, err)
			return fallback
		}
		if n <= 0 {
			errs = append(errs, fmt.Errorf("env %s=%d: value must be > 0", key, n))
			return fallback
		}
		return n
	}

	cfg := &Config{
		Server: Server{
			Addr:            getString("GATEWAY_ADDR", defaultAddr),
			ReadTimeout:     mustDuration("GATEWAY_READ_TIMEOUT", defaultReadTimeout),
			WriteTimeout:    mustDuration("GATEWAY_WRITE_TIMEOUT", defaultWriteTimeout),
			IdleTimeout:     mustDuration("GATEWAY_IDLE_TIMEOUT", defaultIdleTimeout),
			ShutdownTimeout: mustDuration("GATEWAY_SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
		},
		Upstream: Upstream{
			Timeout:          mustDuration("GATEWAY_UPSTREAM_TIMEOUT", defaultUpstreamTimeout),
			URL:              getString("GATEWAY_UPSTREAM_URL", ""),
			MockUpstreamAddr: getString("GATEWAY_MOCK_UPSTREAM_ADDR", defaultMockUpstreamAddr),
		},
		Redis: Redis{
			URL:         getString("GATEWAY_REDIS_URL", "redis://localhost:6379"),
			DialTimeout: mustDuration("GATEWAY_REDIS_DIAL_TIMEOUT", defaultRedisDialTimeout),
			ReadTimeout: mustDuration("GATEWAY_REDIS_READ_TIMEOUT", defaultRedisReadTimeout),
		},
		Postgres: Postgres{
			DSN:            getString("GATEWAY_DB_DSN", "postgres://app:app@localhost:5432/sluice?sslmode=disable"),
			AcquireTimeout: mustDuration("GATEWAY_DB_ACQUIRE_TIMEOUT", defaultPostgresAcquireTimeout),
		},
		Logging: Logging{
			Level:  getString("GATEWAY_LOG_LEVEL", defaultLogLevel),
			Format: getString("GATEWAY_LOG_FORMAT", defaultLogFormat),
		},
		Retry: Retry{
			MaxAttempts: mustPositiveInt("GATEWAY_RETRY_MAX_ATTEMPTS", defaultRetryMaxAttempts),
			BaseDelay:   mustDuration("GATEWAY_RETRY_BASE_DELAY", defaultRetryBaseDelay),
			MaxDelay:    mustDuration("GATEWAY_RETRY_MAX_DELAY", defaultRetryMaxDelay),
			Jitter:      mustFloat("GATEWAY_RETRY_JITTER", defaultRetryJitter),
		},
		Breaker: Breaker{
			Interval:     mustDuration("GATEWAY_BREAKER_INTERVAL", defaultBreakerInterval),
			Timeout:      mustDuration("GATEWAY_BREAKER_TIMEOUT", defaultBreakerTimeout),
			MaxRequests:  uint32(mustPositiveInt("GATEWAY_BREAKER_MAX_REQUESTS", defaultBreakerMaxRequests)),
			MinRequests:  uint32(mustPositiveInt("GATEWAY_BREAKER_MIN_REQUESTS", defaultBreakerMinRequests)),
			FailureRatio: mustFloat("GATEWAY_BREAKER_FAILURE_RATIO", defaultBreakerFailureRatio),
			RetryAfter:   mustDuration("GATEWAY_BREAKER_RETRY_AFTER", defaultBreakerRetryAfter),
		},
		RateLimit: RateLimit{
			RPS:     mustPositiveInt("GATEWAY_RATELIMIT_RPS", defaultRateLimitRPS),
			Burst:   mustPositiveInt("GATEWAY_RATELIMIT_BURST", defaultRateLimitBurst),
			Window:  mustDuration("GATEWAY_RATELIMIT_WINDOW", defaultRateLimitWindow),
			MaxKeys: mustPositiveInt("GATEWAY_RATELIMIT_MAX_KEYS", defaultRateLimitMaxKeys),
		},
		Cache: Cache{
			TTL:          mustDuration("GATEWAY_CACHE_TTL", defaultCacheTTL),
			MaxBodyBytes: mustPositiveInt64("GATEWAY_CACHE_MAX_BODY_BYTES", defaultCacheMaxBodyBytes),
		},
		Metering: Metering{
			BufferSize:    mustPositiveInt("GATEWAY_METERING_BUFFER_SIZE", defaultMeteringBufferSize),
			FlushInterval: mustDuration("GATEWAY_METERING_FLUSH_INTERVAL", defaultMeteringFlushInterval),
		},
		Shutdown: Shutdown{
			HookTimeout: mustDuration("GATEWAY_SHUTDOWN_HOOK_TIMEOUT", defaultShutdownHookTimeout),
		},
		HealthCheckTimeout: mustDuration("GATEWAY_HEALTH_CHECK_TIMEOUT", defaultHealthCheckTimeout),
		WorkerPoolSize:     mustPositiveInt("GATEWAY_WORKER_POOL_SIZE", defaultWorkerPoolSize),
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %w", errs[0])
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// Validate asserts that every boundary timeout is > 0 (NFR-004 / AC-045) and
// that the remaining values are well-formed.
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server addr must not be empty")
	}

	// The boundary timeouts called out by AC-045 (NFR-004).
	timeouts := []struct {
		name  string
		value time.Duration
	}{
		{"server.ReadTimeout", c.Server.ReadTimeout},
		{"server.WriteTimeout", c.Server.WriteTimeout},
		{"server.IdleTimeout", c.Server.IdleTimeout},
		{"upstream.Timeout", c.Upstream.Timeout},
		{"redis.DialTimeout", c.Redis.DialTimeout},
		{"redis.ReadTimeout", c.Redis.ReadTimeout},
		{"postgres.AcquireTimeout", c.Postgres.AcquireTimeout},
		{"healthCheckTimeout", c.HealthCheckTimeout},
		// Retry/breaker timeouts are boundary timeouts too: keeping them in this
		// list satisfies TestConfig_AllBoundariesHaveTimeouts' "any new timeout > 0"
		// contract and the NFR-004 fail-loud guarantee.
		{"retry.BaseDelay", c.Retry.BaseDelay},
		{"retry.MaxDelay", c.Retry.MaxDelay},
		{"breaker.Interval", c.Breaker.Interval},
		{"breaker.Timeout", c.Breaker.Timeout},
		{"breaker.RetryAfter", c.Breaker.RetryAfter},
		{"rateLimit.Window", c.RateLimit.Window},
		{"cache.TTL", c.Cache.TTL},
		{"metering.FlushInterval", c.Metering.FlushInterval},
		{"shutdown.HookTimeout", c.Shutdown.HookTimeout},
	}
	for _, t := range timeouts {
		if t.value <= 0 {
			return fmt.Errorf("timeout %s must be > 0, got %s", t.name, t.value)
		}
	}

	if c.Retry.MaxAttempts < 1 {
		return fmt.Errorf("retry.MaxAttempts must be >= 1, got %d", c.Retry.MaxAttempts)
	}
	if c.Retry.MaxDelay < c.Retry.BaseDelay {
		return fmt.Errorf("retry.MaxDelay (%s) must be >= retry.BaseDelay (%s)", c.Retry.MaxDelay, c.Retry.BaseDelay)
	}
	if c.Retry.Jitter < 0 || c.Retry.Jitter > 1 {
		return fmt.Errorf("retry.Jitter must be in [0,1], got %g", c.Retry.Jitter)
	}
	if c.Breaker.FailureRatio <= 0 || c.Breaker.FailureRatio > 1 {
		return fmt.Errorf("breaker.FailureRatio must be in (0,1], got %g", c.Breaker.FailureRatio)
	}
	if c.Breaker.MinRequests == 0 {
		return fmt.Errorf("breaker.MinRequests must be > 0")
	}
	if c.Breaker.MaxRequests == 0 {
		return fmt.Errorf("breaker.MaxRequests must be > 0")
	}

	if c.RateLimit.RPS <= 0 {
		return fmt.Errorf("rateLimit.RPS must be > 0, got %d", c.RateLimit.RPS)
	}
	if c.RateLimit.Burst <= 0 {
		return fmt.Errorf("rateLimit.Burst must be > 0, got %d", c.RateLimit.Burst)
	}
	if c.RateLimit.MaxKeys <= 0 {
		return fmt.Errorf("rateLimit.MaxKeys must be > 0, got %d", c.RateLimit.MaxKeys)
	}

	if c.Metering.BufferSize <= 0 {
		return fmt.Errorf("metering.BufferSize must be > 0, got %d", c.Metering.BufferSize)
	}

	// Upstream wiring (CARD-013): either an external URL is set (must be a valid
	// absolute http(s) URL — fail loud), or the in-process mock upstream is used
	// and its listen address must be non-empty.
	if c.Upstream.URL != "" {
		u, err := url.Parse(c.Upstream.URL)
		if err != nil {
			return fmt.Errorf("upstream.URL %q is not a valid URL: %w", c.Upstream.URL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("upstream.URL %q must be an http(s) URL, got scheme %q", c.Upstream.URL, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("upstream.URL %q must include a host", c.Upstream.URL)
		}
	} else if c.Upstream.MockUpstreamAddr == "" {
		return fmt.Errorf("upstream.MockUpstreamAddr must not be empty when upstream.URL is unset")
	}

	if c.Server.ShutdownTimeout <= 0 {
		return fmt.Errorf("server.ShutdownTimeout must be > 0, got %s", c.Server.ShutdownTimeout)
	}
	if c.WorkerPoolSize <= 0 {
		return fmt.Errorf("worker pool size must be > 0, got %d", c.WorkerPoolSize)
	}
	switch c.Logging.Format {
	case "json", "text":
	default:
		return fmt.Errorf("log format must be json or text, got %q", c.Logging.Format)
	}

	return nil
}

func getString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// getDuration returns (default, nil) when the env var is unset/empty.
// It returns (default, error) when the var is set but malformed or <= 0,
// so Load() can surface that as a hard failure (NFR-004 fail-loud).
func getDuration(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback, fmt.Errorf("env %s=%q: %w", key, v, err)
	}
	if d <= 0 {
		return fallback, fmt.Errorf("env %s=%q: duration must be > 0", key, v)
	}
	return d, nil
}

// getInt returns (default, nil) when the env var is unset/empty.
// It returns (default, error) when the var is set but unparseable,
// so Load() can surface that as a hard failure (NFR-004 fail-loud).
func getInt(key string, fallback int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback, fmt.Errorf("env %s=%q: %w", key, v, err)
	}
	return n, nil
}

// getFloat returns (default, nil) when the env var is unset/empty.
// It returns (default, error) when the var is set but unparseable,
// so Load() can surface that as a hard failure (NFR-004 fail-loud).
func getFloat(key string, fallback float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback, fmt.Errorf("env %s=%q: %w", key, v, err)
	}
	return f, nil
}

// getInt64 returns (default, nil) when the env var is unset/empty.
// It returns (default, error) when the var is set but unparseable,
// so Load() can surface that as a hard failure (NFR-004 fail-loud).
func getInt64(key string, fallback int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback, fmt.Errorf("env %s=%q: %w", key, v, err)
	}
	return n, nil
}
