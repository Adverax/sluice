# internal/metering

Asynchronous usage metering (COMP-016/017/018, FR-014) — persists a `UsageEvent` per
request to Postgres **without ever blocking the hot path** (INV-003, CON-006).

## Pipeline

```
request (hot path) ──Enqueue──▶ Buffer (chan, cap 1000) ──▶ Worker (1 goroutine) ──▶ MeteringRepository (pgx) ──▶ usage_events
                     non-blocking      drop-on-full           batch + timer flush        bounded retry
```

- **Buffer** (COMP-016, ADR-0005): bounded channel (`GATEWAY_METERING_BUFFER_SIZE`, default 1000).
  `Enqueue` is `select { case ch<-e: default: }` — on full it **drops** and increments
  `metering_events_dropped_total` (via an injected metrics recorder — metering imports no prometheus).
  The request goroutine never waits on the channel, the worker, or Postgres.
- **Worker** (COMP-017): single goroutine; flushes on batch-size OR timer
  (`GATEWAY_METERING_FLUSH_INTERVAL`, default 5s). On Postgres error: bounded retry then
  drop-with-log (AC-037) — the batch is never silently lost; all I/O is off the hot path.
  `Close(ctx)` drains + flushes remaining events (AC-032), registered via `lifecycle.OnShutdown`
  **after** the HTTP drain (so in-flight events are captured) and run with a dedicated hook
  deadline so even a *forced* shutdown still flushes.
- **MeteringRepository** (COMP-018, ADR-0010): port + pgx/v5 adapter over a narrow `Execer`
  (batch INSERT). Schema in `migrations/0001_usage_events.sql`.

## Shutdown design (ADR-0007)

A **stop signal** (a separate channel, closed once) triggers drain — the buffer channel is
**never** closed, so a late `Enqueue` from the hot path can never panic on a closed channel.
This is best-effort, not billing-grade durability (a durable WAL/queue is the documented
production upgrade — backlog).

> Real pgx INSERT against `usage_events` is integration-tested in CARD-011 (testcontainers);
> here the repository is unit-tested with a fake `Execer`.
