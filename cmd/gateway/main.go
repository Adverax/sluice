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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/sony/gobreaker"

	"github.com/adverax/sluice/internal/breaker"
	"github.com/adverax/sluice/internal/cache"
	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/lifecycle"
	"github.com/adverax/sluice/internal/logging"
	"github.com/adverax/sluice/internal/metering"
	"github.com/adverax/sluice/internal/metrics"
	"github.com/adverax/sluice/internal/middleware"
	"github.com/adverax/sluice/internal/pool"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/proxy/resilience"
	"github.com/adverax/sluice/internal/proxy/retry"
	"github.com/adverax/sluice/internal/ratelimit"
	"github.com/adverax/sluice/internal/server"
	"github.com/adverax/sluice/internal/tracing"
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

	// Observability (COMP-013/COMP-014, ADR-0008). The Prometheus registry is
	// constructed here and INJECTED — never the global default — so each process
	// owns its own metric set. The six required metrics (NFR-007/AC-048) register
	// against it via promauto.With(reg) inside metrics.New.
	promRegistry := prometheus.NewRegistry()
	met := metrics.New(promRegistry)

	// OTel tracing. The OTLP/HTTP endpoint comes from GATEWAY_OTEL_ENDPOINT; if
	// unset, a no-op tracer is returned so the gateway runs un-traced. The batch
	// processor exports asynchronously, so a down collector never blocks a request
	// (AC-050). Shutdown flushes pending spans on graceful drain.
	tracer := tracing.New(context.Background(), tracing.Config{
		Endpoint: os.Getenv("GATEWAY_OTEL_ENDPOINT"),
		Insecure: true,
	}, logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown failed", slog.String("error", err.Error()))
		}
	}()

	// Reused upstream HTTP client with a tuned Transport and an explicit total
	// timeout from config (ADR-0010, NFR-004). The HTTPProvider below shares this
	// client, so connection pooling (MaxIdleConns etc.) is process-wide rather
	// than per-request — and is now genuinely EXERCISED rather than discarded.
	httpClient := newUpstreamClient(cfg.Upstream.Timeout)

	// Resolve the mock LLM upstream URL (CARD-013). If GATEWAY_UPSTREAM_URL is set
	// we point the provider at that external endpoint; otherwise we start an
	// in-process mock upstream on a loopback side-listener and target it. Either
	// way the gateway reaches the upstream over REAL HTTP through the pooled
	// client, so pooling + real ctx-cancellation are exercised end-to-end. This
	// is still a MOCK upstream (no real OpenAI/Anthropic — a v1 non-goal).
	upstreamURL, stopMockUpstream, err := resolveUpstream(cfg.Upstream, logger)
	if err != nil {
		return err
	}

	// Model router (FR-002). The "mock" model is served by the HTTPProvider over
	// real HTTP (CARD-013); the in-process provider.Mock remains for fast unit
	// tests elsewhere but is no longer on the RUNNING gateway's path.
	router := proxy.NewRouter()
	router.Register("mock", provider.NewHTTP(httpClient, upstreamURL))
	// Seed breaker_state{provider} to closed (0) at registration so the series
	// is always present in GET /metrics, even for a provider that has never
	// tripped its breaker (AC-048: all six metrics must emit on startup).
	met.SetBreakerState("mock", metrics.BreakerStateClosed)

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
			// breaker_state metric (AC-048): map the gobreaker state to the gauge
			// via the same hook that logs the transition.
			met.SetBreakerState(name, breakerStateValue(to))
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
	//
	// One *Pool instance backs BOTH the unary and the streaming guard so the
	// concurrency cap is shared across paths (NFR-006: bounded upstream goroutines
	// must include streams). The streaming guard releases its slot exactly once
	// when the stream ENDS (channel close / ctx cancel / error) so streams cannot
	// leak slots (CARD-014, AC-014d).
	workerPool := pool.New(cfg.WorkerPoolSize, cfg.Breaker.RetryAfter)
	guardedInfer := workerPool.Guard(composer.InferFunc())

	// Instrument the provider-call seam (COMP-013/COMP-014): time it into
	// provider_request_duration_seconds and wrap it in a nested OTel span that is
	// a child of the request's root span (AC-030). Tracing is OUTERMOST so the
	// span covers the metrics timing too; both decorators preserve the InferFunc
	// signature so the pool→retry→breaker→provider layering is untouched.
	instrumentedInfer := tracer.InstrumentInferFunc(met.InstrumentInferFunc(guardedInfer))

	// Streaming resilience seam (CARD-014, ADR-0006): pool → breaker →
	// provider.InferStream, with NO retry (a partially-sent stream cannot be
	// safely replayed). Stream INITIATION runs through the breaker (open → 503
	// before any SSE byte, AC-014a) and a shared pool slot (saturation → 503,
	// AC-014b); the slot releases when the stream ends (AC-014d). The same
	// instrumentation decorators give streaming provider metric + span parity with
	// unary (AC-014c) — tracing OUTERMOST so its span spans the full stream
	// lifetime including the metrics timing.
	guardedStream := workerPool.GuardStream(composer.StreamFunc())
	instrumentedStream := tracer.InstrumentStreamFunc(met.InstrumentStreamFunc(guardedStream))

	// Async usage metering (COMP-016/COMP-017/COMP-018, FR-014,
	// ADR-0005/ADR-0007/ADR-0010). The Usage Buffer is a bounded channel
	// (GATEWAY_METERING_BUFFER_SIZE, default 1000) that decouples the request hot
	// path from Postgres: the server enqueues a UsageEvent after each completed
	// inference via a NON-BLOCKING send — drop-on-full so the hot path never
	// blocks (INV-003 / CON-006). The dropped counter is incremented through the
	// injected metrics recorder (metering never imports Prometheus, ADR-0008).
	// The Metering Worker batch-reads from the buffer and flushes through the
	// pgx/v5 MeteringRepository (reusing the same pgPool); its Close is registered
	// as a lifecycle shutdown hook AFTER the HTTP drain so remaining buffered
	// events are flushed before exit (AC-032).
	meteringBuffer := metering.NewBuffer(cfg.Metering.BufferSize, met)
	meteringRepo := metering.NewPgxRepository(pgPool)
	meteringWorker := metering.NewWorker(meteringBuffer, meteringRepo, logger,
		metering.WithFlushInterval(cfg.Metering.FlushInterval),
	)
	meteringWorker.Start()

	// HTTP boundary: implement the generated StrictServerInterface (ADR-0011)
	// and register all routes on appMux via api.HandlerFromMux (CON-001). The
	// Prometheus registry is injected so GET /metrics serves it (COMP-013). The
	// metering sink is injected so completed inferences are recorded async
	// (FR-014).
	srv := server.New(router, healthHandler, logger,
		server.WithInferFunc(instrumentedInfer),
		server.WithStreamFunc(instrumentedStream),
		server.WithMetricsRegistry(promRegistry),
		server.WithMeteringSink(meteringBuffer),
	)
	appMux := http.NewServeMux()
	appHandler := srv.Handler(appMux)

	httpServer := &http.Server{
		Addr:         cfg.Server.Addr,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	manager := lifecycle.New(httpServer, logger, cfg.Server.ShutdownTimeout,
		// Give each OnShutdown hook (e.g. the metering worker's Close) its own
		// independent deadline so a forced HTTP drain does not starve the flush
		// (GATEWAY_SHUTDOWN_HOOK_TIMEOUT, default 5s — AC-032 / FR-012).
		lifecycle.WithHookTimeout(cfg.Shutdown.HookTimeout),
	)

	// Flush remaining buffered usage events on shutdown, AFTER the HTTP drain so
	// no new events are being enqueued by the time Close drains the buffer
	// (AC-032 / FR-012). The hook runs with its own fresh-deadline context.
	manager.OnShutdown(meteringWorker.Close)

	// Stop the in-process mock upstream (if started) on graceful shutdown, after
	// the HTTP drain so no in-flight inference is still calling it (CARD-013).
	if stopMockUpstream != nil {
		manager.OnShutdown(stopMockUpstream)
	}

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
		// ratelimit_rejected_total is incremented at the 429 reject path via the
		// injected recorder (the middleware never imports Prometheus, ADR-0008).
		middleware.WithRejectRecorder(met),
	)

	// Response cache (COMP-004, FR-005, ADR-0004/ADR-0010). The repository is the
	// go-redis adapter behind the CacheRepository port, reusing the same
	// redisClient as the rate limiter and health checker. The middleware acts
	// ONLY on POST /v1/chat/completions, bypasses streaming requests, and falls
	// through to the live handler on any Redis error (a cache blip must never
	// become a client error — AC-017). The default TTL comes from config
	// (GATEWAY_CACHE_TTL, default 5m); X-Cache-TTL overrides it per request.
	cacheRepo := cache.NewRedisRepository(redisClient)
	cacheMW := middleware.NewCacheMiddleware(cacheRepo, cfg.Cache.TTL, logger,
		middleware.WithMaxBodyBytes(cfg.Cache.MaxBodyBytes),
	)

	// Composition order (ADR-0006), outermost first:
	//   recover → logging → tracing → metrics → rate-limit → counting → cache → routes.
	// Panic recovery is OUTERMOST so a handler panic is translated to a 500 and
	// the process survives (AC-033); it wraps logging (which logs the panic at
	// ERROR and re-panics — AC-041). Tracing creates the root span before the
	// metrics/rate-limit work so the nested provider span is its child (AC-030).
	// The metrics middleware records http_requests_total + duration and the
	// inflight gauge around the whole inner chain. Rate-limit still runs BEFORE
	// any provider/pool work so a 429 never reaches the proxy (INV-004), and the
	// counting middleware drains in-flight requests on shutdown (FR-012/NFR-005).
	// The cache middleware is INNERMOST (just before the generated routes): a HIT
	// short-circuits the provider path while still being covered by the outer
	// logging/metrics/tracing instrumentation (FR-005).
	handler := middleware.Recoverer(logger)(
		logging.Middleware(logger)(
			middleware.Tracing(tracer.Tracer())(
				met.Middleware(
					rateLimiter.Middleware(
						manager.CountingMiddleware(
							cacheMW.Middleware(appHandler),
						),
					),
				),
			),
		),
	)
	httpServer.Handler = handler

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return manager.Run(ctx)
}

// breakerStateValue maps a gobreaker.State onto the breaker_state gauge value
// (0=closed, 1=half-open, 2=open) so the per-provider circuit state is
// queryable in Prometheus.
func breakerStateValue(s gobreaker.State) float64 {
	switch s {
	case gobreaker.StateHalfOpen:
		return metrics.BreakerStateHalfOpen
	case gobreaker.StateOpen:
		return metrics.BreakerStateOpen
	default:
		return metrics.BreakerStateClosed
	}
}

// resolveUpstream decides the mock LLM upstream URL the HTTPProvider targets
// (CARD-013). When cfg.URL is set it is returned verbatim with a nil stop
// function (the upstream is external — the gateway owns nothing to shut down).
// When cfg.URL is empty it starts an in-process mock upstream on a loopback
// side-listener (cfg.MockUpstreamAddr, default 127.0.0.1:0), returns the
// resolved http://host:port URL, and a stop function that gracefully shuts the
// side-server down (wired as a lifecycle OnShutdown hook).
func resolveUpstream(cfg config.Upstream, logger *slog.Logger) (string, func(context.Context) error, error) {
	if cfg.URL != "" {
		logger.Info("using external mock upstream", slog.String("url", cfg.URL))
		return cfg.URL, nil, nil
	}

	ln, err := net.Listen("tcp", cfg.MockUpstreamAddr)
	if err != nil {
		return "", nil, fmt.Errorf("mock upstream listen on %q: %w", cfg.MockUpstreamAddr, err)
	}

	mockServer := &http.Server{
		Handler:           provider.MockUpstreamHandler(provider.MockUpstreamOptions{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if serveErr := mockServer.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			logger.Error("mock upstream server failed", slog.String("error", serveErr.Error()))
		}
	}()

	url := "http://" + ln.Addr().String()
	logger.Info("started in-process mock upstream", slog.String("addr", ln.Addr().String()), slog.String("url", url))

	stop := func(ctx context.Context) error {
		return mockServer.Shutdown(ctx)
	}
	return url, stop, nil
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
