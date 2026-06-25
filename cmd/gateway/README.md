# cmd/gateway

Entry point for the sluice gateway service. It wires all components together
via dependency injection (ADR-0008) and boots a production-ready HTTP server
with structured logging, health/readiness probes, OpenAPI request validation,
resilience (retry + circuit breaker), and graceful shutdown.

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
8. **Resilience composition (ADR-0006, FR-006/FR-007):**
   ```
   retry.New(cfg.Retry, retry.WithNonRetryable(resilience.IsOpenState))
   breaker.NewRegistry(cfg.Breaker, breaker.WithOnStateChange(logTransition))
   resilience.New(retrier, breakers, cfg.Breaker.RetryAfter)
   ```
   Composition order: `retry( breaker.Execute( providerCall ) )`. The retry engine
   treats `ErrOpenState` as non-retryable so it never spins against an open breaker.
9. `server.New(router, healthHandler, logger, server.WithInferFunc(composer.InferFunc()))` →
   `srv.Handler(appMux)` — builds the generated `api.StrictServerInterface` with the
   composed resilience call injected, then wraps all routes with kin-openapi
   request-validation (ADR-0011).
10. Build `http.Server` with all timeouts from config (NFR-004).
11. `lifecycle.New(...)` — wraps the server with graceful-drain logic (FR-012).
12. Compose middleware (outermost first): `logging.Middleware` → `CountingMiddleware` → routes.
13. `signal.NotifyContext` on SIGINT/SIGTERM → `manager.Run(ctx)`.

## Environment variables

See `internal/config/README.md` for the full list of `GATEWAY_*` variables,
including `GATEWAY_RETRY_*` and `GATEWAY_BREAKER_*` knobs added in CARD-007.

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Clean graceful drain. |
| `1` | Config error, listen failure, or shutdown timeout exceeded. |
