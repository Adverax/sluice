# How sluice works

A layered, code-grounded account of how the **sluice** LLM gateway actually works
internally — for architects, developers, and SRE/operators who need to understand the
mechanism, not just the API.

sluice is a **single Go service** (CON-003 — logical bounded contexts, not microservices)
that sits in front of LLM providers and adds the resilience and observability a gateway
needs before it carries real traffic. It is **OpenAI-compatible end-to-end** (ADR-0012/0013):
a request enters through one drop-in `POST /v1/chat/completions` front door (any OpenAI SDK
works unmodified), is routed to a provider by its `model`, and is protected on the way out by
rate limiting, a bounded worker pool, retries, and a circuit breaker before the
OpenAI-compatible upstream adapter forwards it (Ollama / OpenAI / vLLM by config; an in-process
mock by default) — while usage is metered asynchronously and the whole path is instrumented.

> These docs are **generated from ground truth** — the domain model (`meta/architecture/`)
> and the **real source code**. Each aspect's `README.md` carries a **doc→code map** so the
> narrative stays anchored to the files it describes. They document **shipped** behavior only.

## The four aspects (bounded contexts)

| Group | Aspect | What it covers | Source contexts |
|-------|--------|----------------|-----------------|
| [surface-api](01-surface-api/) | [Proxy](01-surface-api/proxy/) | The request hot path: routing, JSON + SSE streaming, response cache, client cancellation, and the process lifecycle (graceful shutdown, panic recovery). | CTX-001 (CAP-001, CAP-005) |
| [resilience](02-resilience/) | [Resilience](02-resilience/resilience/) | Protecting the path and upstreams: per-key rate limiting (local + Redis token bucket), the bounded worker pool / backpressure, and the per-provider circuit breaker. | CTX-002 (CAP-002, CAP-003) |
| [operations](03-operations/) | [Observability](03-operations/observability/) | Liveness/readiness probes, the Prometheus metric catalog, OpenTelemetry tracing, and structured per-request logging. | CTX-003 (CAP-004) |
| [integrations](04-integrations/) | [Metering](04-integrations/metering/) | Asynchronous usage recording to Postgres via a bounded buffer + batch worker, off the hot path. | CTX-004 (CAP-006) |

## How the aspects relate

The proxy is the customer; resilience, metering, and observability supply it (from
`contexts.yml` → `context_map`):

```
                 ┌─────────────────── Observability (CTX-003) ───────────────────┐
                 │  conformist: reads in-flight/outcome/breaker/drop via metrics  │
                 ▼                         ▼                          ▼
   client →  Proxy (CTX-001) ──sync──▶ Resilience (CTX-002) ──▶ provider (EXT-001, ACL)
                 │  customer_supplier: rate-limit + breaker on the hot path
                 └──events (one-way buffer)──▶ Metering (CTX-004) ──batch──▶ Postgres (EXT-003)
```

- **Proxy → Resilience** — synchronous on the hot path (rate-limit + breaker gate the call). *(DEC-006)*
- **Proxy → Metering** — one-way enqueue to a Go channel; the hot path never blocks on Postgres (INV-003). *(DEC-007)*
- **Observability → everything** — conformist; reads state via a shared in-process Prometheus registry. *(DEC-008)*
- **Proxy/Resilience/Metering → external systems** — each behind an anti-corruption layer (Provider interface, Redis repo, pgx repo). *(DEC-009/010)*

## Reading routes

- 🏛 **Architect** — start here, then the C4 model in [`meta/architecture/c4/`](../../meta/architecture/c4/) and each aspect's `README.md` (doc→code map + ADR links). The context relationships above come from [`contexts.yml`](../../meta/architecture/domain/contexts.yml).
- 💻 **Developer** — follow a request: [Proxy · inference proxying](01-surface-api/proxy/01-inference-proxying.md) → [Resilience](02-resilience/resilience/) → [Metering](04-integrations/metering/01-usage-metering.md) → [Observability](03-operations/observability/). For the runtime: [Proxy · runtime lifecycle](01-surface-api/proxy/02-runtime-lifecycle.md).
- 🔧 **SRE / Operator** — [Observability](03-operations/observability/) (health, metrics, tracing) → [Proxy · runtime lifecycle](01-surface-api/proxy/02-runtime-lifecycle.md) (graceful shutdown) → [Resilience](02-resilience/resilience/) (backpressure, breaker). For task-level operation, see the [operator role docs](../role/operator/).

## Related documentation

- **End-user / product docs:** [`docs/role/`](../role/) — what an [integrator](../role/integrator/) or [operator](../role/operator/) *does* with the system.
- **Architecture decisions:** [`meta/architecture/decisions/adr/`](../../meta/architecture/decisions/adr/).
- **C4 diagrams:** [`meta/architecture/c4/`](../../meta/architecture/c4/) (context, containers, per-context components).
