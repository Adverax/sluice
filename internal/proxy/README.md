# internal/proxy

## Purpose

Model→provider routing registry (FR-002). The `Router` is a ports-and-adapters
seam that maps a request's `model` field onto a registered `provider.Provider`
(the ADR-0009 anti-corruption port) without importing any concrete provider
package. The routing/registry concern lives here, isolated from the HTTP mapping
and inference logic in `internal/server`.

## Architecture

```
cmd/gateway  ──Register(model, p)──▶  Router  ──Provider(model)──▶  server
                 (startup)                           (per request)
```

`Router` depends only on the `provider.Provider` interface from
`internal/provider`. It is safe for concurrent use: `Register` is called once at
startup; `Provider` is called on every inbound request.

## Key types

| Type | Description |
|------|-------------|
| `Router` | Concurrent-safe model→provider registry. Construct with `NewRouter`. |
| `ErrModelNotRegistered` | Sentinel returned when no provider matches the requested model. Callers map it to HTTP 404 (AC-007). Match with `errors.Is`. |

## See also

- `internal/provider` — the `Provider` interface and canonical request/response types
- `internal/server` — consults `Router.Provider` on every inference request
