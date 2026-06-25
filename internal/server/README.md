# internal/server

## Purpose

Concrete implementation of the OpenAPI-generated `api.StrictServerInterface`
(ADR-0011, contract-first). It owns the behaviour behind the generated HTTP
boundary: maps generated DTOs ↔ canonical `provider.Request/Response`
(ADR-0009 ACL), routes by model via `proxy.Router` (FR-002), and translates
`health.Result` onto the spec's readiness schema (FR-009).

`Handler()` also wires a kin-openapi request-validation middleware
(`openapi3filter`) in front of all routes: unknown enum values and missing
required fields are rejected with a structured `400` response **before** any
handler code runs (ADR-0011).

## Architecture

```
http.Handler (kin-openapi validator)
    └─ api.HandlerFromMux (generated routes)
           └─ api.NewStrictHandler
                  └─ Server  ──proxy.Router──▶ provider.Provider
                             ──health.Handler──▶ Result
                             ──InferFunc seam──▶ resilience.Composer (ADR-0006)
```

`Server` has no env-var dependencies; it is fully configured by the wiring in
`cmd/gateway`.

## Key types

| Type | Description |
|------|-------------|
| `Server` | Implements `api.StrictServerInterface`. Holds the model router, health handler, logger, and infer hook. Construct with `New`. |
| `InferFunc` | `func(ctx, provider.Provider, provider.Request) (provider.Response, error)` — wrap point for retry/circuit-breaker decorators (FR-007, ADR-0006). Default: direct `p.Infer` call. |
| `ErrServiceUnavailable` | Sentinel that `internal/proxy/resilience.Unavailable` satisfies via `errors.Is`. Defined here to avoid an import cycle (resilience imports server, not the reverse). Handler maps any matching error to 503 + `Retry-After`. |
| `Option` | Functional option for `New`; currently `WithInferFunc`. |

## Endpoints

| Route | AC | Notes |
|-------|----|-------|
| `POST /v1/chat/completions` | AC-001 (200), AC-003/019/021 (502), AC-004/006 (400), AC-007 (404), AC-020/022 (503) | OpenAPI validator fires first; resilience errors checked before generic 502 |
| `GET /healthz` | AC-025 | Always 200 while the process is alive |
| `GET /readyz` | AC-026/027/028 | 200 all-ok, 503 with per-dependency map |
| `GET /metrics` | — | Stub; returns 200 empty body until COMP-013 |

## See also

- `internal/proxy` — `Router` used for model dispatch
- `internal/proxy/resilience` — provides the composed `InferFunc` and `Unavailable` error
- `internal/health` — `Handler.Evaluate` drives `/readyz`
- `internal/api` — generated types and `StrictServerInterface`
- `internal/provider` — canonical `Request`/`Response`/`Provider` types
- ADR-0006: `meta/architecture/decisions/adr/0006-proxy-resilience-integration-hybrid.md`
