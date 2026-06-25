# cmd/gateway

Entry point for the sluice gateway service. It wires all CARD-001 components
together via dependency injection (ADR-0008) and boots a production-ready HTTP
server with structured logging, health probes, and graceful shutdown.

Business behaviour (proxy, rate limiting, cache, circuit breaker, metering,
metrics) is delivered by later cards.

## Startup sequence

1. `config.Load()` — reads env vars, validates, fails loudly on bad values.
2. `logging.New(...)` — constructs the injected `slog.Logger`.
3. `health.New(...)` — creates the liveness/readiness handler.
4. Build `http.Server` with all timeouts from config.
5. `lifecycle.New(...)` — wraps the server with graceful-drain logic.
6. Mount routes:
   - `GET /healthz` → `health.Live` (not counted as in-flight)
   - `GET /readyz` → `health.Ready` (not counted as in-flight)
   - `/` → `CountingMiddleware(appMux)` (in-flight tracked)
7. Wrap the outer mux with `logging.Middleware` (access log).
8. `signal.NotifyContext` on SIGINT/SIGTERM → `manager.Run(ctx)`.

## Environment variables

See `internal/config/README.md` for the full list of `GATEWAY_*` variables.

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Clean graceful drain. |
| `1` | Config error, listen failure, or shutdown timeout exceeded. |
