# internal/api

**Generated** HTTP API boundary for sluice — the contract-first layer (ADR-0011).

The source of truth is [`api/openapi.yaml`](../../api/openapi.yaml) (OpenAPI 3.0.3).
`api.gen.go` is produced from it by `oapi-codegen` (v2, `std-http-server` + `models` +
`strict-server`) — **do not edit it by hand**. Regenerate with:

```sh
make generate        # = go generate ./...
```

Generation is reproducible and the committed output must be diff-clean (CI will enforce
this in a later card). The tool is pinned via `tools.go` (`//go:build tools`) and `go.mod`.

## What it provides

- Request/response/error types: `ChatCompletionRequest`, `ChatCompletionResponse`,
  `Usage`, `Message`, role enum, `Error`, `HealthStatus`, `ReadinessStatus`.
- `StrictServerInterface` — the typed operation interface handlers implement.
- `NewStrictHandler` + `HandlerFromMux` — registers routes on a stdlib `*http.ServeMux`
  (no web framework, per CON-001).

## Who implements it

CARD-003 (non-streaming proxy, router, health) and CARD-004 (streaming) implement
`StrictServerInterface`, mapping these public DTOs ↔ the canonical `internal/provider`
types (the ACL, ADR-0009). Note: the public `temperature` is `float32` while the canonical
type is `float64` — the handler mapping converts.

> Streaming (`stream:true` → `text/event-stream`) cannot be fully expressed in OpenAPI 3.0;
> it is documented in the spec and implemented as SSE in CARD-004.
