# CARD-010: Async usage metering → Postgres

**Status:** ready
**Priority:** P2
**Category:** feature
**Estimate:** 2d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/010-async-usage-metering-postgres
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-4
**Depends on:** CARD-003, CARD-001
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Implement async usage metering under `internal/metering/**.go` as COMP-016 Usage Buffer, COMP-017 Metering Worker, COMP-018 MeteringRepository.

**Usage Buffer (COMP-016):**
- Buffered Go channel with capacity 1000 (ADR-0005: buffer_1000).
- Non-blocking send: use `select { case ch <- event: default: }` — drop on full (AC-036, INV-003).
- On drop: increment `metering_events_dropped_total` Prometheus counter; the hot path must **never** block (CON-006).

**Metering Worker (COMP-017):**
- Background goroutine: batch-reads events from the channel, flushes to Postgres via MeteringRepository.
- Two flush triggers: batch size reached OR periodic timer (e.g., every 5s).
- **Graceful shutdown:** on SIGTERM (after HTTP drain), flush the remaining events in the buffer before exit (AC-032 — FR-012 final metering-flush AC, POL-005). ADR-0007: buffered_channel_drop_on_full.
- **Shutdown timeout forced:** if in-flight requests do not complete within the shutdown timeout, process is forcibly terminated; log the count of unfinished requests (AC-051).

**MeteringRepository (COMP-018):**
- Interface + pgx/v5 implementation; batch INSERT to `usage_events` table (fields: provider, model, tokens, latency, status).
- ADR-0010: repository interface; pgx injected.
- Postgres unavailable at flush time: log error, do not silently lose the batch (retry or drop with log); **hot path is not blocked** (AC-037).

## Acceptance criteria

### FR-014 — Async usage persistence

**AC-035**
- **Given:** API Client has made 100 requests, metering buffer is not full
- **When:** background worker flushes a batch
- **Then:** 100 records appear in the `usage_events` table in Postgres
- **Test:** `TestMetering_AsyncFlush_PersistsRecords` (kind: happy)

**AC-036**
- **Given:** metering buffer is full (capacity=N), the (N+1)th request arrives
- **When:** gateway finishes processing the (N+1)th request
- **Then:** the (N+1)th record is dropped, the `metering_events_dropped_total` counter increments by 1; hot path is not blocked
- **Test:** `TestMetering_BufferFull_DropsWithoutBlocking` (kind: boundary)

**AC-037**
- **Given:** Postgres is unavailable at flush time
- **When:** background worker attempts to INSERT a batch
- **Then:** error is logged, batch is not silently lost (retry or drop with log), hot path is not blocked
- **Test:** `TestMetering_PostgresDown_NoHotpathBlock` (kind: error)

### FR-012 — Graceful shutdown (metering-flush AC)

**AC-032**
- **Given:** metering buffer holds 50 usage events at the moment SIGTERM is received
- **When:** process finishes draining in-flight requests
- **Then:** all 50 usage events are flushed to Postgres before exit
- **Test:** `TestGracefulShutdown_FlushesMetering` (kind: happy)

**AC-051**
- **Given:** in-flight requests do not complete within the shutdown timeout (e.g. 30s)
- **When:** shutdown timeout elapses
- **Then:** process is forcibly terminated; in-flight requests receive a connection error; log contains the count of unfinished requests
- **Test:** `TestGracefulShutdown_TimeoutForced` (kind: boundary)

## Architecture context

- **FR:** FR-014, FR-012 (metering-flush AC and timeout AC)
- **NFR:** —
- **ADR:** ADR-0007, ADR-0005, ADR-0010
- **Components:** COMP-016 Usage Buffer, COMP-017 Metering Worker, COMP-018 MeteringRepository
- **Trace:** meta/architecture/trace.yml

## Worktree notes

Implemented on branch `card/010-async-metering`.

**Packages / files**
- `internal/metering/` (new): `metering.go` (UsageEvent, Sink/NopSink ports,
  DropRecorder port, MeteringRepository port), `buffer.go` (COMP-016 Usage
  Buffer), `worker.go` (COMP-017 Metering Worker), `pgxrepo.go` (COMP-018 pgx/v5
  adapter over a narrow `Execer` interface). Tests: `buffer_test.go`,
  `worker_test.go`, `pgxrepo_test.go`, `metering_test.go` (fakes).
- `internal/metrics/metrics.go`: added `metering_events_dropped_total` counter +
  `IncMeteringEventsDropped` on the Recorder port / NopRecorder.
- `internal/config/config.go`: `Metering{BufferSize,FlushInterval}` from
  `GATEWAY_METERING_BUFFER_SIZE` (default 1000, ADR-0005, fail-loud >0) and
  `GATEWAY_METERING_FLUSH_INTERVAL` (default 5s).
- `internal/server/server.go`: `WithMeteringSink` option; enqueue a UsageEvent
  after a successful unary infer (status 200) and after stream completion
  (terminal chunk usage, best-effort). Non-blocking.
- `internal/lifecycle/lifecycle.go`: `OnShutdown` post-drain hooks; forced
  shutdown (AC-051) logs `forced shutdown: N requests unfinished` on
  `context.DeadlineExceeded` and returns nil; clean drain still logs
  `drained N requests`.
- `cmd/gateway/main.go`: builds Buffer+Worker+PgxRepository (reusing pgPool),
  injects Sink into the server, registers `worker.Close` via `OnShutdown`
  AFTER the HTTP drain (AC-032).
- `migrations/0001_usage_events.sql` (new): `usage_events` DDL. CARD-011's
  `make up` applies it; live pgx INSERT is integration-tested there
  (testcontainers) — deferred here, repository unit-tested with a fake Execer.

**Design / flush triggers**
- Buffer: bounded channel; `Enqueue` is `select { case ch<-e: default: drop }`
  (ADR-0007 drop-on-full) and increments the dropped counter via the injected
  DropRecorder — the hot path NEVER blocks (INV-003 / CON-006).
- Worker: single goroutine; flush triggers = batch size reached OR periodic
  timer. All persistence + bounded retry runs in the worker goroutine.
- Postgres-down (AC-037): bounded retry (default 2 extra attempts) with logged
  failures, then drop-with-log — batch never silently lost; hot path unaffected.
- Shutdown flush (AC-032): `Close(ctx)` signals stop, the worker flushes the
  in-flight batch then drains all still-buffered events, then exits; `Close`
  waits on a `done` channel (ctx-bounded; no goroutine leak, idempotent).

**Ports & adapters**: metering imports neither prometheus nor a concrete pgx
client (only `pgx.Batch`/`BatchResults` via a narrow `Execer`). Mirrors the
ratelimit/breaker recorder injection (ADR-0008) and ADR-0010.

**AC → test**
- AC-035 → `TestMetering_AsyncFlush_PersistsRecords` (+ `_BatchSizeTrigger`)
- AC-036 → `TestMetering_BufferFull_DropsWithoutBlocking`
- AC-037 → `TestMetering_PostgresDown_NoHotpathBlock` (+ `_AlwaysFails_DropsWithLog`)
- AC-032 → `TestGracefulShutdown_FlushesMetering` (+ lifecycle
  `TestOnShutdown_HookRunsAfterDrain`)
- AC-051 → `TestGracefulShutdown_TimeoutForced` (clean-drain tests stay green)
- Server wiring → `TestProxy_UnaryEnqueuesUsage`,
  `TestProxy_ProviderError_NoUsageEnqueued`
- Repository → `TestPgxRepository_Flush_*` (fake Execer/BatchResults)

**Status**: `go build ./...`, `go vet ./...`, `go test -race ./...` all green;
`go generate ./...` diff-clean (api.gen.go untouched); `go mod tidy` no change
(pgx/v5 already a dep). golangci-lint: only a PRE-EXISTING `SA1019`
(`api.GetSwagger`) in untouched server code; new code clean.
