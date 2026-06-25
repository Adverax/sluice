# cmd/gateway

Entry point for the sluice gateway service. It wires all components together
via dependency injection (ADR-0008) and boots a production-ready HTTP server
with structured logging, health/readiness probes, OpenAPI request validation,
and graceful shutdown.

Business behaviour (rate limiting, cache, circuit breaker, metering, metrics)
is delivered by later cards.

## Startup sequence

1. `config.Load()` — reads env vars, validates, fails loudly on bad values.
2. `logging.New(...)` — constructs the injected `slog.Logger`.
3. `newUpstreamClient(cfg.Upstream.Timeout)` — pooled `*http.Client` shared by
   all provider adapters (ADR-0010); handed to real adapters in later cards.
4. `proxy.NewRouter()` + `router.Register("mock", ...)` — model→provider
   registry (FR-002); mock provider keeps the proxy path exercisable until real
   adapters land.
5. `health.New(logger, cfg.HealthCheckTimeout)` — liveness/readiness handler
   with per-check deadline from `GATEWAY_HEALTH_CHECK_TIMEOUT`.
6. `newRedisClient(cfg.Redis)` + `newPostgresPool(cfg.Postgres)` — real
   dependency clients; deferred close registered immediately.
7. `healthHandler.Register(NewRedisChecker(...), NewPostgresChecker(...))` —
   wires real ping checks for `/readyz` (AC-027/AC-028).
8. `server.New(router, healthHandler, logger)` → `srv.Handler(appMux)` —
   builds the generated `api.StrictServerInterface` implementation and wraps
   all routes with a kin-openapi request-validation middleware (ADR-0011);
   invalid requests are rejected 400 before reaching any handler.
9. Build `http.Server` with all timeouts from config (NFR-004).
10. `lifecycle.New(...)` — wraps the server with graceful-drain logic (FR-012).
11. Compose middleware: `logging.Middleware` → `CountingMiddleware` → routes.
12. `signal.NotifyContext` on SIGINT/SIGTERM → `manager.Run(ctx)`.

All routes (including `/healthz` and `/readyz`) are registered through the
generated `api.HandlerFromMux` boundary and served through the validation
middleware. Probe requests are not separately counted as in-flight because the
generated strict handler handles them before `CountingMiddleware` would act.

## Environment variables

See `internal/config/README.md` for the full list of `GATEWAY_*` variables.

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Clean graceful drain. |
| `1` | Config error, listen failure, or shutdown timeout exceeded. |
