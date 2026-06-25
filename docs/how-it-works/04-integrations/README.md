# integrations — where data leaves the gateway

How sluice records usage to durable storage without ever blocking the request path.
This group covers the asynchronous metering pipeline: a one-way enqueue from the proxy
into a bounded buffer, a background worker that batches and flushes to Postgres, and
drop-on-full semantics that keep the hot path isolated (INV-003).

| Aspect | Topics |
|--------|--------|
| [Metering](metering/) | [01 · Usage metering](metering/01-usage-metering.md) — buffer, batch worker, drop-on-full, batch INSERT, shutdown flush |

Bounded context: **CTX-004 Metering** (CAP-006). It receives a one-way event stream from
the [proxy](../01-surface-api/) (events integration) and persists to Postgres behind a pgx
repository ACL; its buffer occupancy and drop counter are surfaced by
[observability](../03-operations/) (`metering_buffer_size`, `metering_events_dropped_total`).

> Redis (response cache + distributed rate limit) and the LLM provider are also external
> integrations, but their adapters live with the contexts that own them — see
> [proxy](../01-surface-api/) (cache, provider) and [resilience](../02-resilience/) (Redis rate limit).
