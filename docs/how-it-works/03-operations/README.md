# operations — running and observing the gateway

How an operator sees inside sluice and confirms it is healthy: liveness/readiness probes,
the Prometheus metric catalog, OpenTelemetry tracing, and structured per-request logging.
This group is the cross-cutting observation layer — it reads the state of the other
contexts rather than owning business state of its own.

| Aspect | Topics |
|--------|--------|
| [Observability](observability/) | [01 · Health & readiness](observability/01-health-and-readiness.md) · [02 · Metrics](observability/02-metrics.md) — the injected registry + metric catalog · [03 · Tracing & logging](observability/03-tracing-and-logging.md) |

Bounded context: **CTX-003 Observability** (CAP-004). It is a *conformist* to
[proxy](../01-surface-api/), [resilience](../02-resilience/), and
[integrations](../04-integrations/) — it instruments them via a shared in-process
Prometheus registry. For task-level operation, see the [operator role docs](../../role/operator/).
