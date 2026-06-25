# internal/health

Implements the liveness probe (`GET /healthz`, FR-008) and readiness probe
(`GET /readyz`, FR-009) for the gateway. Readiness is built on a small
`Checker` port so dependency checks can be added without modifying this package.

## Key types

| Type | Description |
|------|-------------|
| `Handler` | Aggregates `Checker` instances; exposes `Evaluate`, `Live`, and `Ready`. Construct with `New`. |
| `Result` | Transport-agnostic readiness verdict: `Healthy bool` + `Dependencies map[string]string`. Returned by `Evaluate`; the HTTP boundary maps it to a status code and body. |
| `Checker` (interface) | Readiness port: `Name() string` + `Check(ctx) error`. |
| `CheckerFunc` | Adapter that promotes a plain function into a `Checker`. |
| `RedisPinger` | Narrow port (`Ping(ctx) *redis.StatusCmd`). `*redis.Client` satisfies it. |
| `PostgresPinger` | Narrow port (`Ping(ctx) error`). `*pgxpool.Pool` satisfies it. |

## Key functions

`New(logger, timeout) *Handler` — `timeout` is the per-check deadline (defaults
to 2 s when `<= 0`); set via `GATEWAY_HEALTH_CHECK_TIMEOUT` in `cmd/gateway`.
Checkers are added at startup with `Handler.Register(...)`.

`Evaluate(ctx) Result` — runs all registered checkers **concurrently**, each
under its own deadline derived from `timeout`. Returns an aggregated `Result`.
One slow checker cannot starve the rest; no goroutine outlives the derived
context.

`NewRedisChecker(RedisPinger) Checker` — pings Redis; failure surfaces as the
`"redis"` dependency reason in `/readyz` (AC-027).

`NewPostgresChecker(PostgresPinger) Checker` — pings the pgx pool; failure
surfaces as the `"postgres"` dependency reason in `/readyz` (AC-028).

## Probe behaviour

| Endpoint | Healthy response | Unhealthy response |
|----------|------------------|--------------------|
| `GET /healthz` | `200 {"status":"ok"}` | — (always 200 while process is alive) |
| `GET /readyz` | `200 {"status":"ok","dependencies":{...}}` | `503 {"status":"unavailable","dependencies":{...}}` |

With no checkers registered, `/readyz` returns 200.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GATEWAY_HEALTH_CHECK_TIMEOUT` | `3s` | Per-check deadline for `/readyz` dependency checks |

(Loaded by `internal/config`, passed to `health.New` in `cmd/gateway`.)

## See also

- `internal/config` — `Config.HealthCheckTimeout` field
- `internal/server` — calls `Handler.Evaluate` to serve `GET /readyz`
