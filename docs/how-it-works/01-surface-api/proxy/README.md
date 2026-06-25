# Proxy (CTX-001) — how it works

The **Proxy** bounded context is the hot-path core of sluice: it accepts an
inbound HTTP inference request, routes it to a provider by the `model` field,
looks the response up in Redis, proxies the result (buffered JSON or an SSE
stream), retries transient upstream failures, and cancels the upstream call when
the client disconnects. The same context also owns the **process lifecycle** —
graceful shutdown and panic recovery — because both operate directly on
in-flight request state.

This aspect documents the two capabilities the context ships:

| Capability | Topic | What it covers |
|------------|-------|----------------|
| CAP-001 Inference proxying | [01-inference-proxying.md](01-inference-proxying.md) | The `POST /v1/chat/completions` hot path: middleware chain → router → resilience seam → provider; non-streaming JSON vs SSE streaming; response caching; retry-with-backoff; client-cancellation. |
| CAP-005 Runtime lifecycle | [02-runtime-lifecycle.md](02-runtime-lifecycle.md) | Graceful shutdown (signal → drain → shutdown hooks → flushed log) and panic recovery middleware. |

## File table

| File | Purpose |
|------|---------|
| [01-inference-proxying.md](01-inference-proxying.md) | L4 narrative of the request hot path (CAP-001). |
| [02-runtime-lifecycle.md](02-runtime-lifecycle.md) | L4 narrative of shutdown + panic recovery (CAP-005). |
| [diagrams/01-inference-proxying-01.puml](diagrams/01-inference-proxying-01.puml) | Non-streaming request sequence. |
| [diagrams/01-inference-proxying-02.puml](diagrams/01-inference-proxying-02.puml) | SSE streaming initiation + per-chunk flush sequence. |
| [diagrams/02-runtime-lifecycle-01.puml](diagrams/02-runtime-lifecycle-01.puml) | Graceful-shutdown sequence. |
| [diagrams/02-runtime-lifecycle-02.puml](diagrams/02-runtime-lifecycle-02.puml) | Panic-recovery state diagram. |

## doc → code map

Built from the CTX-001 components in `meta/architecture/trace.yml` and their real
source files.

| Source file | Component | Role |
|-------------|-----------|------|
| `internal/server/server.go` | COMP-001 HTTP Handler & Router, COMP-002 Proxy Core | Implements the generated `api.StrictServerInterface` (ADR-0011): validates the body, routes by model, maps DTOs onto canonical `provider.Request/Response` (ADR-0009), drives the `InferFunc`/`StreamFunc` seams, maps errors to 503/502/400/404. Owns `streamResponse` (SSE forwarding). |
| `internal/proxy/router.go` | COMP-001/COMP-002 | `Router` — model → `provider.Provider` registry (FR-002); returns `ErrModelNotRegistered` → 404. |
| `internal/proxy/resilience/resilience.go` | (composition seam, ADR-0006) | `Composer` wires `retry(breaker.Execute(call))` into `server.InferFunc`, and `breaker.ExecuteStream(...)` (no retry) into `server.StreamFunc`; maps open breaker / deadline to `*Unavailable` (→ 503). |
| `internal/proxy/retry/retry.go` | COMP-003 Retry Engine | Bounded, deadline-aware exponential backoff + jitter (FR-006); typed retryable classification; `ErrExhausted` → 502. |
| `internal/provider/provider.go` | COMP-005 Provider Interface | The single `Provider` ACL (ADR-0009): `Infer`/`InferStream`, canonical `Request`/`Response`/`Chunk`/`Usage`, `StatusError`. |
| `internal/provider/httpprovider.go` | COMP-005 | `HTTPProvider` — HTTP/SSE adapter over a pooled `*http.Client`; `http.NewRequestWithContext` gives ctx-cancellation; maps non-2xx to `StatusError`. |
| `internal/provider/mock.go`, `internal/provider/mockupstream.go` | COMP-005 | In-process `Mock` provider and `MockUpstreamHandler` (the v1 mock upstream; no real OpenAI/Anthropic). |
| `internal/cache/cache.go`, `internal/cache/redisrepo.go` | COMP-004 Cache Adapter | `CacheRepository` port (ADR-0010) + go-redis/v9 `RedisRepository`. |
| `internal/middleware/cache.go` | COMP-004 | `CacheMiddleware` — sha256 key, 5-min default TTL + `X-Cache-TTL` override (ADR-0004), `X-Cache` header, streaming bypass, fall-through on Redis error. |
| `internal/lifecycle/lifecycle.go` | COMP-006 Lifecycle Manager | `Manager` — runs `http.Server`, graceful drain on signal, `OnShutdown` hooks, `CountingMiddleware`, drained/flushed log. |
| `internal/middleware/recover.go` | COMP-007 Panic Recovery | `Recoverer` middleware (recover → 500; `http.ErrAbortHandler` passthrough) and `SafeGo`. |
| `cmd/gateway/main.go` | COMP-006 (+ all CTX-001 wiring) | Composition root: builds the middleware chain in ADR-0006 order and wires the `InferFunc`/`StreamFunc` seams, cache, metering sink, and lifecycle hooks. |

> The HTTP boundary itself (`internal/api/api.gen.go`) is **generated** from the
> OpenAPI contract (ADR-0011) and is not documented here — only the behaviour
> behind that contract is.

## Related docs

- **ADRs:**
  [ADR-0009 Single Provider Interface](../../../../meta/architecture/decisions/adr/0009-single-provider-interface.md) ·
  [ADR-0011 API contract-first with OpenAPI](../../../../meta/architecture/decisions/adr/0011-api-contract-first-openapi.md) ·
  [ADR-0006 Proxy↔Resilience integration (hybrid)](../../../../meta/architecture/decisions/adr/0006-proxy-resilience-integration-hybrid.md) ·
  [ADR-0010 Repository interface per context](../../../../meta/architecture/decisions/adr/0010-repository-interface-per-context.md) ·
  [ADR-0004 Cache TTL default 5 min + per-request override](../../../../meta/architecture/decisions/adr/0004-cache-ttl-default-5min-per-request-override.md)
- **Other aspects:**
  [Resilience](../../02-resilience/resilience/) (rate limit, worker pool, circuit breaker — the seam the proxy calls) ·
  [Metering](../../04-integrations/metering/) (the async usage sink the proxy enqueues to) ·
  [Observability](../../03-operations/observability/) (the metrics/tracing/logging wrapping the chain)
- **Integrator role docs (end-user):** [docs/role/integrator/](../../../role/integrator/) —
  in particular [chat-completions.md](../../../role/integrator/chat-completions.md),
  [streaming.md](../../../role/integrator/streaming.md),
  [caching.md](../../../role/integrator/caching.md), and
  [errors-and-resilience.md](../../../role/integrator/errors-and-resilience.md).
