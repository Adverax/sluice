# internal/health

Implements the liveness probe (`GET /healthz`, FR-008) and readiness probe
(`GET /readyz`, FR-009) for the gateway. Readiness is built on a small
`Checker` port so CARD-003 can register real Redis/Postgres checks without
modifying this package.

## Key types

| Type | Description |
|------|-------------|
| `Handler` | Aggregates `Checker` instances and exposes `Live` and `Ready` HTTP handlers. Construct with `New`. |
| `Checker` (interface) | Readiness port: `Name() string` + `Check(ctx) error`. |
| `CheckerFunc` | Adapter that promotes a plain function into a `Checker`. |

## Key function

`New(logger, timeout) *Handler` — `timeout` bounds each individual readiness
check (defaults to 2 s when `<= 0`). Checkers are added later with
`Handler.Register(...)`.

## Probe behaviour

| Endpoint | Healthy response | Unhealthy response |
|----------|------------------|--------------------|
| `GET /healthz` | `200 {"status":"ok"}` | — (always 200 while process is alive) |
| `GET /readyz` | `200 {"status":"ok","dependencies":{...}}` | `503 {"status":"unavailable","dependencies":{...}}` |

With no checkers registered, `/readyz` returns 200. Real dependency checks
(Redis, Postgres) are wired in CARD-003.

## Gateway wiring

`/healthz` and `/readyz` are mounted on the outer mux **before**
`CountingMiddleware` so probe traffic is never counted as in-flight application
requests.
