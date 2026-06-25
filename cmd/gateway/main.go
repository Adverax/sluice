// Command gateway is the sluice service entry point. It wires configuration,
// the structured logger, the proxy router + generated HTTP boundary
// (ADR-0011), the health/readiness framework with real Redis/Postgres ping
// checkers, and the lifecycle manager together via dependency injection
// (ADR-0008), then boots a timeout-bearing http.Server with graceful shutdown
// (FR-001/FR-002/FR-008/FR-009/FR-012, NFR-004, NFR-005, FR-016). Rate
// limiting, cache, breaker, metering and metrics are delivered by later cards.
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/sony/gobreaker"

	"github.com/adverax/sluice/internal/breaker"
	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/lifecycle"
	"github.com/adverax/sluice/internal/logging"
	"github.com/adverax/sluice/internal/middleware"
	"github.com/adverax/sluice/internal/pool"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/proxy/resilience"
	"github.com/adverax/sluice/internal/proxy/retry"
	"github.com/adverax/sluice/internal/ratelimit"
	"github.com/adverax/sluice/internal/server"
)

func main() {
	if err := run(); err != nil {
		// run() already logs the detail; emit a last-resort line and exit 1.
		slog.Error("gateway exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// run performs the full DI wiring and blocks until graceful shutdown completes.
// It returns nil on a clean drain (exit code 0).
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(os.Stdout, cfg.Logging.Format, cfg.Logging.Level)
	logger.Info("starting gateway",
		slog.String("addr", cfg.Server.Addr),
		slog.String("log_format", cfg.Logging.Format),
		slog.String("log_level", cfg.Logging.Level),
	)

	// Reused upstream HTTP client with a tuned Transport and an explicit total
	// timeout from config (ADR-0010, NFR-004). Real provider adapters (later
	// cards) share this client; constructing it here keeps connection pooling
	// process-wide rather than per-request.
	httpClient := newUpstreamClient(cfg.Upstream.Timeout)
	_ = httpClient // handed to real adapters in later cards; built now per ADR-0010.

	// Model router (FR-002). Real provider adapters land in later cards; until
	// then a Mock provider keeps the proxy path exercisable end-to-end.
	router := proxy.NewRouter()
	router.Register("mock", provider.New(provider.WithResponse(provider.Response{
		Model:        "mock",
		Content:      "this is a mock completion",
		FinishReason: "stop",
	})))

	// Health/readiness framework (FR-008/FR-009) with REAL dependency checkers.
	// Use the dedicated per-check timeout (cfg.HealthCheckTimeout) rather than
	// reusing cfg.Redis.DialTimeout so the readiness probe SLA is tunable
	// independently of the Redis connection timeout (NFR-004).
	healthHandler := health.New(logger, cfg.HealthCheckTimeout)

	redisClient, err := newRedisClient(cfg.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = redisClient.Close() }()

	pgPool, err := newPostgresPool(context.Background(), cfg.Postgres)
	if err != nil {
		return err
	}
	defer pgPool.Close()

	healthHandler.Register(
		health.NewRedisChecker(redisClient),
		health.NewPostgresChecker(pgPool),
	)

	// Resilience composition (FR-006/FR-007, ADR-0006): retry(breaker.Execute(
	// providerCall)). The retry engine treats an open breaker as non-retryable so
	// it never spins against ErrOpenState; the breaker is per-provider (keyed by
	// model). The composed call is injected into the server's InferFunc seam
	// (ADR-0006) so CARD-008's worker pool can wrap it later without changes.
	retrier := retry.New(cfg.Retry, retry.WithNonRetryable(resilience.IsOpenState))
	breakers := breaker.NewRegistry(cfg.Breaker,
		breaker.WithOnStateChange(func(name string, from, to gobreaker.State) {
			logger.Info("circuit breaker state change",
				slog.String("provider", name),
				slog.String("from", from.String()),
				slog.String("to", to.String()),
			)
		}),
	)
	composer := resilience.New(retrier, breakers, cfg.Breaker.RetryAfter)

	// Worker pool / backpressure (COMP-010, FR-015, ADR-0006). The pool wraps the
	// composed resilience InferFunc so the layering is pool → retry → breaker →
	// provider: the pool acquires a slot at the ENTRY of the provider-call path
	// (reject-before-work) and caps concurrent upstream calls at
	// cfg.WorkerPoolSize (GATEWAY_WORKER_POOL_SIZE, default 100 — ADR-0003). When
	// saturated it returns ErrPoolSaturated, which the server maps to 503 +
	// Retry-After (the same 503 path as the resilience fast-fail). The signature
	// is unchanged so CARD-005's rate-limit middleware can sit OUTSIDE this layer.
	guardedInfer := pool.Guard(cfg.WorkerPoolSize, cfg.Breaker.RetryAfter, composer.InferFunc())

	// HTTP boundary: implement the generated StrictServerInterface (ADR-0011)
	// and register all routes on appMux via api.HandlerFromMux (CON-001).
	srv := server.New(router, healthHandler, logger, server.WithInferFunc(guardedInfer))
	appMux := http.NewServeMux()
	appHandler := srv.Handler(appMux)

	httpServer := &http.Server{
		Addr:         cfg.Server.Addr,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	manager := lifecycle.New(httpServer, logger, cfg.Server.ShutdownTimeout)

	// Rate-limit middleware (COMP-008/COMP-009, FR-004, ADR-0001/ADR-0006/ADR-0010).
	// The LOCAL per-key token-bucket registry is the fast in-process path; the
	// DISTRIBUTED RateLimitRepository (the Redis adapter, reusing the same
	// redisClient as the health checker) enforces the shared cross-instance cap.
	// On a Redis error the middleware FAILS OPEN to the local limiter (a Redis
	// blip must not 429/503 the whole fleet — see internal/middleware docs).
	// The registry is bounded by MaxKeys (GATEWAY_RATELIMIT_MAX_KEYS) with
	// LRU-style eviction and a periodic idle-sweep; Close stops its goroutine.
	rlRegistry := ratelimit.NewRegistry(cfg.RateLimit.RPS, cfg.RateLimit.Burst,
		ratelimit.WithMaxKeys(cfg.RateLimit.MaxKeys),
	)
	defer rlRegistry.Close()
	rlRepo := ratelimit.NewRedisRepository(redisClient)
	rateLimiter := middleware.NewRateLimiter(
		rlRegistry, rlRepo, cfg.RateLimit.RPS, cfg.RateLimit.Window, logger,
	)

	// Composition order (ADR-0006), outermost first: request logging wraps the
	// rate-limit middleware (which must run BEFORE any work, so a 429 never
	// reaches the proxy/pool/provider — INV-004), which wraps the in-flight
	// counting middleware, which wraps the generated route handler so proxy
	// requests are drained on shutdown (FR-012 / NFR-005).
	handler := logging.Middleware(logger)(
		rateLimiter.Middleware(
			manager.CountingMiddleware(appHandler),
		),
	)
	httpServer.Handler = handler

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return manager.Run(ctx)
}

// newUpstreamClient builds the reused outbound HTTP client for provider calls
// (ADR-0010, NFR-004). The Transport is tuned for connection reuse and the
// client carries an explicit total timeout from config.
func newUpstreamClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// newRedisClient builds a go-redis client from config, applying the dial/read
// timeouts (NFR-004). The URL is parsed and the configured timeouts override
// any defaults so the readiness ping is bounded.
func newRedisClient(cfg config.Redis) (*redis.Client, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	opts.DialTimeout = cfg.DialTimeout
	opts.ReadTimeout = cfg.ReadTimeout
	return redis.NewClient(opts), nil
}

// newPostgresPool builds a pgx pool from config, applying the acquire/connect
// timeout (NFR-004). Pool creation is lazy with respect to the actual TCP
// connection, so this does not block on Postgres being up at boot.
func newPostgresPool(ctx context.Context, cfg config.Postgres) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, err
	}
	poolCfg.ConnConfig.ConnectTimeout = cfg.AcquireTimeout
	return pgxpool.NewWithConfig(ctx, poolCfg)
}
