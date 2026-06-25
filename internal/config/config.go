// Package config loads and validates the gateway service configuration from
// the environment. All values have defaults so the service is bootable without
// any environment variables set. The GATEWAY_ prefix matches ADR-0003 and the
// developer Makefile.
package config

import (
	"fmt"
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

	defaultRedisDialTimeout = 5 * time.Second
	defaultRedisReadTimeout = 3 * time.Second

	defaultPostgresAcquireTimeout = 5 * time.Second

	defaultWorkerPoolSize = 100

	defaultLogLevel  = "info"
	defaultLogFormat = "json"
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

// Upstream holds timeouts for the outbound provider boundary (the proxy itself
// is CARD-002+; only the timeout-bearing config lives here).
type Upstream struct {
	// Timeout is the total request timeout for the upstream http.Client.
	Timeout time.Duration
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

// Config is the fully-resolved service configuration.
type Config struct {
	Server   Server
	Upstream Upstream
	Redis    Redis
	Postgres Postgres
	Logging  Logging

	// WorkerPoolSize is consumed by CARD-008; loaded here for completeness.
	WorkerPoolSize int
}

// Load reads the configuration from the environment, applies defaults for any
// unset value, and validates it. It returns an error if any value is invalid.
func Load() (*Config, error) {
	cfg := &Config{
		Server: Server{
			Addr:            getString("GATEWAY_ADDR", defaultAddr),
			ReadTimeout:     getDuration("GATEWAY_READ_TIMEOUT", defaultReadTimeout),
			WriteTimeout:    getDuration("GATEWAY_WRITE_TIMEOUT", defaultWriteTimeout),
			IdleTimeout:     getDuration("GATEWAY_IDLE_TIMEOUT", defaultIdleTimeout),
			ShutdownTimeout: getDuration("GATEWAY_SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
		},
		Upstream: Upstream{
			Timeout: getDuration("GATEWAY_UPSTREAM_TIMEOUT", defaultUpstreamTimeout),
		},
		Redis: Redis{
			URL:         getString("GATEWAY_REDIS_URL", "redis://localhost:6379"),
			DialTimeout: getDuration("GATEWAY_REDIS_DIAL_TIMEOUT", defaultRedisDialTimeout),
			ReadTimeout: getDuration("GATEWAY_REDIS_READ_TIMEOUT", defaultRedisReadTimeout),
		},
		Postgres: Postgres{
			DSN:            getString("GATEWAY_DB_DSN", "postgres://app:app@localhost:5432/sluice?sslmode=disable"),
			AcquireTimeout: getDuration("GATEWAY_DB_ACQUIRE_TIMEOUT", defaultPostgresAcquireTimeout),
		},
		Logging: Logging{
			Level:  getString("GATEWAY_LOG_LEVEL", defaultLogLevel),
			Format: getString("GATEWAY_LOG_FORMAT", defaultLogFormat),
		},
		WorkerPoolSize: getInt("GATEWAY_WORKER_POOL_SIZE", defaultWorkerPoolSize),
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

	// The six timeouts called out by AC-045.
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
	}
	for _, t := range timeouts {
		if t.value <= 0 {
			return fmt.Errorf("timeout %s must be > 0, got %s", t.name, t.value)
		}
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

func getDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func getInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
