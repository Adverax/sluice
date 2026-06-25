# 01 — Usage metering (CAP-006)

> **Aspect:** Metering (CTX-004) · **Capability:** CAP-006 Usage metering · **Requirement:** FR-014
> **Code:** `internal/metering/` · `internal/server/server.go` (enqueue) · `cmd/gateway/main.go` (wiring) · `migrations/0001_usage_events.sql`

This is the mechanism behind CAP-006: how Sluice records one usage row per completed
inference into Postgres **without the request ever waiting on the database**. Everything
below is grounded in the real source under `internal/metering/`; file paths are absolute
to that package unless noted.

---

## 1. Why async — the hot path must never block on Postgres

The governing invariant is **INV-003 / CON-006**: the request hot path must never block
on writing usage data. A synchronous `INSERT` after every request would couple response
latency to Postgres health — a slow or down database would stall (or fail) live traffic.
ADR-0007 rejected both that and the naive `go func(){ ... }()` per-event approach (which
violates the no-goroutine-leak constraint NFR-003), and chose a **bounded buffered channel
+ single background worker** with **drop-on-full** semantics.

The shape of the pipeline (see [diagram 01](diagrams/01-usage-metering-01.puml)):

```
request (hot path) ──Enqueue──▶ Buffer (chan, cap 1000) ──▶ Worker (1 goroutine) ──▶ MeteringRepository (pgx) ──▶ usage_events
                     non-blocking      drop-on-full           batch + timer flush        bounded retry
```

The whole `metering` package is built around one promise, stated in the package doc
(`metering.go`):

> The hot path MUST NEVER block on metering (INV-003 / CON-006): when the buffer is full
> the event is DROPPED and a counter is incremented (ADR-0007 …, AC-036).

It also stays free of infrastructure types: the package imports **neither Prometheus nor a
concrete pgx pool** (ADR-0008 boundary hygiene). Metrics are reached through narrow ports
(`DropRecorder`, `BufferSizeRecorder`); persistence through `MeteringRepository` (ADR-0010).

---

## 2. The canonical event — `UsageEvent`

The unit that crosses every stage is `UsageEvent` (`metering.go`). It is deliberately
provider-agnostic (ADR-0009) so the metering context never imports a provider package:

```go
type UsageEvent struct {
	Provider string // routing key / model alias from the router (FR-002)
	Model    string // model that produced the completion (response.Model)
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Latency time.Duration // wall-clock the gateway spent serving
	Status  int           // HTTP status returned (e.g. 200)
	RequestID string       // correlates with logs/traces; may be empty
	Timestamp time.Time    // request completion time
}
```

> **Note (doc↔model):** the domain model names the aggregate `UsageRecord` (AGG-004) and
> mentions a `cost` field; the shipped struct is named `UsageEvent` and carries **no cost
> column** — only the token/latency/status fields above mirror the `usage_events` table.
> Cost is **not determinable from code** (not implemented).

---

## 3. The one-way enqueue from the proxy (INV-007)

The producer lives in CTX-001 (the proxy/server), not in the metering context — the only
coupling between them is a one-way write into the buffer. After a **completed** inference
the server builds a `UsageEvent` and enqueues it. From `internal/server/server.go`
(`recordUsage`, unary path):

```go
s.meter.Enqueue(metering.UsageEvent{
	Provider:         routeKey,      // model alias used for routing (FR-002)
	Model:            model,         // resolved response model
	PromptTokens:     usage.PromptTokens,
	CompletionTokens: usage.CompletionTokens,
	TotalTokens:      usage.TotalTokens,
	Latency:          latency,
	Status:           status,        // 200 on the success path
	RequestID:        logging.RequestIDFromContext(ctx),
	Timestamp:        time.Now(),
})
```

The streaming path has its own `recordUsage` (on `streamResponse`) that does the same from
the terminal chunk's usage after the stream completes; it short-circuits if no sink is wired
(`if r.meter == nil { return }`).

The server depends only on the `Sink` port (`metering.go`) — a deliberately one-method
interface so the server "never sees the buffer/worker machinery":

```go
type Sink interface {
	// MUST return immediately and MUST NOT block the caller (the request hot path):
	// on a full buffer the event is dropped (AC-036, INV-003 / CON-006).
	Enqueue(e UsageEvent)
}
```

`*Buffer` satisfies `Sink`; a `NopSink` lets the server run un-metered (tests) without a
nil check. INV-007 ("exactly one UsageRecord per handled request unless the buffer is
full") is realised by exactly one `recordUsage` call on each completed path; a
provider-error path returns before reaching it, so failed inferences enqueue nothing
(see `TestProxy_ProviderError_NoUsageEnqueued`).

---

## 4. The bounded buffer — non-blocking publish, drop-on-full

`Buffer` (COMP-016, `buffer.go`) is just a buffered channel plus a drop counter:

```go
type Buffer struct {
	ch       chan UsageEvent
	recorder DropRecorder
}
```

The capacity is **1000** by default (ADR-0005), sourced from config
(`GATEWAY_METERING_BUFFER_SIZE`); `NewBuffer` floors a non-positive value to 1 so the
channel is always usable. The entire design rests on `Enqueue` being a **non-blocking send**:

```go
func (b *Buffer) Enqueue(e UsageEvent) {
	select {
	case b.ch <- e:
		// queued for the worker.
	default:
		// buffer full: drop and count (ADR-0007 drop-on-full).
		b.recorder.IncMeteringEventsDropped()
	}
}
```

The `select { case ...: default: }` is the load-bearing line: if the channel is full the
`default` arm runs immediately, the event is discarded, and `metering_events_dropped_total`
is incremented through the injected `DropRecorder`. The caller (the request goroutine)
**never waits** on the channel, the worker, or Postgres (AC-036). This is why a sustained
non-zero `metering_events_dropped_total` is the operator's signal that the buffer/flush
throughput needs tuning (ADR-0005/0007 both call this out).

`Buffer` exposes `Events() <-chan UsageEvent` (receive side for the worker) and `Len()` (the
occupancy read used for the gauge and tests). Drops are counted via the narrow port —
`metering` never imports Prometheus:

```go
type DropRecorder interface { IncMeteringEventsDropped() }
```

`*metrics.Metrics` satisfies it in production; `NopDropRecorder{}` is the default so the
buffer needs no nil check.

---

## 5. The background worker — batch + timer flush (POL-004)

`Worker` (COMP-017, `worker.go`) is a **single goroutine** that owns all persistence and all
retry/error handling — none of it ever touches the hot path. Defaults:

```go
const (
	defaultBatchSize     = 100
	defaultFlushInterval = 5 * time.Second
	defaultFlushTimeout  = 5 * time.Second   // bounds a single Flush call
	defaultFlushRetries  = 2                 // additional attempts after first failure
	defaultRetryBackoff  = 50 * time.Millisecond
)
```

The loop (`run`) selects over three sources and flushes on **whichever trigger fires first**
(POL-004) — batch fills *or* the ticker fires (see [diagram 02](diagrams/01-usage-metering-02.puml)):

```go
for {
	select {
	case e, ok := <-w.events:
		if !ok { flush(); return }      // channel closed (not used in normal op)
		batch = append(batch, e)
		w.publishBufferSize()           // gauge tracks occupancy
		if len(batch) >= w.batchSize { flush() }
	case <-ticker.C:                    // periodic flush so events aren't held long
		w.publishBufferSize()
		flush()
	case <-w.stop:                      // graceful shutdown — see §7
		flushed := w.flush(batch)
		flushed += w.drain()
		atomic.StoreInt64(&w.shutdownFlushed, int64(flushed))
		w.publishBufferSize()
		return
	}
}
```

`flush` is a no-op on an empty batch and resets the slice afterward. The timer trigger acts
as the Scheduler actor (ACT-003) so events are not held arbitrarily long under light load.

### 5.1 The `metering_buffer_size` gauge

`publishBufferSize` reports current occupancy after every dequeue/flush and each tick, via
the injected `BufferSizeRecorder` — again no Prometheus import in this package:

```go
func (w *Worker) publishBufferSize() {
	w.bufRecorder.SetMeteringBufferSize(w.buf.Len())
}
```

The read is a cheap `len(chan)` and never blocks the worker. The initial (empty) occupancy
is published once at loop start "so the gauge exists from the start". `*metrics.Metrics`
is injected via `WithBufferSizeRecorder` in `cmd/gateway/main.go`; the default is
`NopBufferSizeRecorder{}`.

### 5.2 Flush with bounded retry (AC-037)

`flush` copies the batch (so the caller can reuse its backing array), then retries up to
`flushRetries` times before a **drop-with-log** — the batch is never silently lost:

```go
for attempt := 0; attempt <= w.flushRetries; attempt++ {
	if attempt > 0 && w.retryBackoff > 0 { time.Sleep(w.retryBackoff) }
	ctx, cancel := context.WithTimeout(context.Background(), w.flushTimeout)
	err := w.repo.Flush(ctx, events)
	cancel()
	if err == nil { return len(events) }
	lastErr = err
	w.logger.LogAttrs(..., "metering flush failed", slog.Int("attempt", attempt+1), ...)
}
// retries exhausted: log loudly and drop the batch (AC-037), hot path unaffected.
w.logger.LogAttrs(..., "metering batch dropped after retries", slog.Int("batch_size", len(events)), ...)
return 0
```

Because all of this runs in the worker goroutine, a down or slow Postgres **cannot** block
or fail a live request (FR-014 / AC-037: log error, do not block the hot path, retry or drop
with log). The per-flush `flushTimeout` also guarantees a hung Postgres can never wedge the
worker (and thereby the shutdown drain) indefinitely.

---

## 6. The repository ACL — batch INSERT via pgx (ADR-0010)

`MeteringRepository` (COMP-018) is the persistence port; no pgx types leak into the worker
or the domain. The pgx/v5 adapter `PgxRepository` (`pgxrepo.go`) depends only on a narrow
`Execer` interface (which `*pgxpool.Pool` satisfies, as do `*pgx.Conn` and `pgx.Tx`):

```go
type Execer interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}
```

`Flush` pipelines the whole batch in **one network round-trip** using a `pgx.Batch` of
parameterised single-row INSERTs (no string building → no SQL injection):

```go
const insertSQL = `INSERT INTO usage_events
	(provider, model, prompt_tokens, completion_tokens, total_tokens, latency_ms, status, request_id, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

func (r *PgxRepository) Flush(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 { return nil }
	batch := &pgx.Batch{}
	for _, e := range events {
		batch.Queue(insertSQL, e.Provider, e.Model, e.PromptTokens, e.CompletionTokens,
			e.TotalTokens, e.Latency.Milliseconds(), e.Status, e.RequestID, e.Timestamp)
	}
	results := r.db.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()
	for i := range events {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("metering: insert usage event %d/%d: %w", i+1, len(events), err)
		}
	}
	return nil
}
```

Note `e.Latency.Milliseconds()` — the `time.Duration` is stored as the `latency_ms BIGINT`
column. The target table (`migrations/0001_usage_events.sql`) mirrors the struct exactly:

```sql
CREATE TABLE IF NOT EXISTS usage_events (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider          TEXT        NOT NULL,
    model             TEXT        NOT NULL,
    prompt_tokens     INTEGER     NOT NULL,
    completion_tokens INTEGER     NOT NULL,
    total_tokens      INTEGER     NOT NULL,
    latency_ms        BIGINT      NOT NULL,
    status            INTEGER     NOT NULL,
    request_id        TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS usage_events_created_at_idx ON usage_events (created_at);
```

`Flush` returns an error if any row fails so the worker's bounded retry (§5.2) applies; it
honours `ctx` for cancellation (the worker bounds it with `flushTimeout`).

---

## 7. Graceful shutdown — the final flush (POL-005 / AC-032)

On graceful shutdown the worker must not lose buffered-but-unflushed events. `cmd/gateway/main.go`
wires `meteringWorker.Close` as a lifecycle shutdown hook registered **after** the HTTP drain,
so by the time it runs no new events are being enqueued:

```go
// Flush remaining buffered usage events on shutdown, AFTER the HTTP drain so
// no new events are being enqueued by the time Close drains the buffer (AC-032 / FR-012).
manager.OnShutdown(meteringWorker.Close)
```

`Close(ctx)` closes the dedicated `stop` channel (idempotently) and then waits on either the
worker finishing (`w.done`) or its own deadline:

```go
func (w *Worker) Close(ctx context.Context) error {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		w.logger.LogAttrs(..., "metering worker close timed out before final flush completed", ...)
		return ctx.Err()
	}
}
```

**Why a separate `stop` channel and not closing the buffer?** (ADR-0007) The buffer channel
is *never* closed, because a late `Enqueue` from the hot path on a closed channel would
panic. Only the lifecycle manager closes `stop`, and only the worker reads it — safe by
construction. On `stop` the `run` loop flushes the in-flight batch, then `drain` empties the
channel and flushes the remainder in batch-sized chunks, recording the total in
`shutdownFlushed` (an `atomic.Int64`).

That total surfaces to the operator via `FlushedOnShutdown()`:

```go
func (w *Worker) FlushedOnShutdown() int { return int(atomic.LoadInt64(&w.shutdownFlushed)) }
```

`main.go` injects it into the lifecycle so the shutdown log reads "flushed M usage events"
alongside "drained N requests" (AC-015c):

```go
lifecycle.WithFlushedCountFn(meteringWorker.FlushedOnShutdown),
```

### 7.1 Its own deadline

The hook does **not** share the HTTP drain budget. `main.go` gives each `OnShutdown` hook an
independent deadline via `lifecycle.WithHookTimeout(cfg.Shutdown.HookTimeout)`
(`GATEWAY_SHUTDOWN_HOOK_TIMEOUT`, default 5s) "so a forced HTTP drain does not starve the
flush". Even a forced shutdown therefore gets a fresh budget to flush.

> **Honest limitation (ADR-0007):** this is best-effort, not billing-grade durability. Under
> sustained overload events are dropped (counted), and a hard kill (SIGKILL) loses whatever
> is still buffered. A durable WAL/queue is the documented production upgrade.

---

## 8. End-to-end summary

1. A completed inference (unary or stream) calls `Server.recordUsage`, which builds a
   `UsageEvent` and calls `Sink.Enqueue` — the only coupling from the proxy to metering.
2. `Buffer.Enqueue` does a non-blocking channel send; on a full buffer the event is dropped
   and `metering_events_dropped_total` is incremented. The request returns regardless.
3. The single `Worker` goroutine drains the channel into a batch and flushes on batch-size
   **or** timer (POL-004), publishing `metering_buffer_size` as it goes.
4. `PgxRepository.Flush` batch-INSERTs the events into `usage_events` in one pgx round-trip;
   on error the worker retries with backoff, then drops-with-log (AC-037).
5. On SIGTERM the lifecycle runs `Worker.Close` after the HTTP drain, on its own deadline;
   the worker drains and flushes the remainder, and the count is logged as
   "flushed M usage events" (POL-005 / AC-032 / AC-015c).

See the [aspect README](README.md) for the file table, the doc→code map, and related docs.
