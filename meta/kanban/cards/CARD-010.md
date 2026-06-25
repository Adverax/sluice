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

—
