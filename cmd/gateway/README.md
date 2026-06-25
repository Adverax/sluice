# cmd/gateway

Entry point for the sluice gateway service. It wires all components together
via dependency injection (ADR-0008) and boots a production-ready HTTP server
with structured logging, observability (Prometheus + OTel tracing), health/readiness
probes, OpenAPI request validation, resilience (retry + circuit breaker), and graceful
shutdown.

## Startup sequence

1. `config.Load()` — reads env vars, validates, fails loudly on bad values.
2. `logging.New(...)` — constructs the injected `slog.Logger`.
3. **Observability (COMP-013/COMP-014):**
   - `prometheus.NewRegistry()` + `metrics.New(reg)` — six required metrics registered
     against the injected registry (never the global default — ADR-0008).
   - `tracing.New(ctx, Config{Endpoint: GATEWAY_OTEL_ENDPOINT}, logger)` — OTel
     OTLP/HTTP provider; no-op fallback when the env var is unset. Batch exporter
     runs off-path so a down collector never blocks requests (AC-050). `defer
     tracer.Shutdown(...)` flushes spans on graceful drain.
4. `newUpstreamClient(cfg.Upstream.Timeout)` — pooled `*http.Client` shared by all
   provider adapters (ADR-0010).
5. `proxy.NewRouter()` + `router.Register("mock", ...)` — model→provider registry
   (FR-002). `met.SetBreakerState("mock", BreakerStateClosed)` seeds the
   `breaker_state{provider="mock"}` series to `0` at registration so it appears in
   `/metrics` immediately (AC-048).
6. `health.New(...)` + real Redis/Postgres checkers wired for `/readyz` (AC-027/028).
7. **Resilience composition (ADR-0006, FR-006/FR-007):**
   ```
   retry.New(cfg.Retry, WithNonRetryable(IsOpenState))
   breaker.NewRegistry(cfg.Breaker, WithOnStateChange(logTransition + met.SetBreakerState))
   resilience.New(retrier, breakers, cfg.Breaker.RetryAfter)
   ```
   Composition order: `retry( breaker.Execute( providerCall ) )`. The retry engine
   treats `ErrOpenState` as non-retryable so it never spins against an open breaker.
   The `OnStateChange` hook updates `breaker_state` via the injected `*Metrics`.
8. `pool.Guard(...)` — backpressure cap (COMP-010, FR-015, ADR-0003); wraps the
   resilience `InferFunc`.
9. **InferFunc instrumentation:**
   ```go
   tracer.InstrumentInferFunc(met.InstrumentInferFunc(guardedInfer))
   ```
   Tracing is outermost (spans cover metrics timing); both decorators preserve the
   `InferFunc` signature (AC-030).
10. `server.New(router, healthHandler, logger, WithInferFunc(...), WithMetricsRegistry(reg))` —
    generated `StrictServerInterface`; `GetMetrics` serves `promhttp.HandlerFor(reg, ...)`
    so `/metrics` exposes the injected registry, not the global default.
11. Build `http.Server` with all timeouts from config (NFR-004).
12. `lifecycle.New(...)` — wraps the server with graceful-drain logic (FR-012).
13. **Middleware chain (outermost first):**
    ```
    recover → logging → tracing → metrics → rate-limit → counting → routes
    ```
    - `Recoverer` is outermost: catches panics that escape the logging middleware's
      re-raise, emits 500, process survives (AC-033).
    - `Tracing` creates the root span before metrics/rate-limit so the provider span
      is its child (AC-030).
    - `met.Middleware` records `http_requests_total`, `http_request_duration_seconds`,
      and `gateway_inflight_requests`.
    - Rate-limit runs before any provider/pool work (INV-004); reports rejections via
      the injected `metrics.Recorder` (ADR-0008).
14. `signal.NotifyContext` on SIGINT/SIGTERM → `manager.Run(ctx)`.

## Environment variables

See `internal/config/README.md` for the full list of `GATEWAY_*` variables.
Key additions for observability:

| Variable | Description |
|----------|-------------|
| `GATEWAY_OTEL_ENDPOINT` | OTLP/HTTP collector `host:port`. Unset → no-op tracing. |

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Clean graceful drain. |
| `1` | Config error, listen failure, or shutdown timeout exceeded. |
