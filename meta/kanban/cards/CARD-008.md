# CARD-008: Bounded worker pool & backpressure

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 2d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/008-worker-pool-backpressure
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-3
**Depends on:** CARD-003
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Implement bounded concurrency to upstream providers under `internal/pool/**.go` as COMP-010 Worker Pool / Backpressure.

- **Pool size:** read from env `GATEWAY_WORKER_POOL_SIZE`; default 100 (ADR-0003: env_configurable).
- **Semaphore-based implementation:** use a buffered channel semaphore or `golang.org/x/sync/errgroup.SetLimit` to cap the number of concurrent goroutines waiting on upstream.
- **Saturation path:** when the pool is full and a new request arrives, immediately return 503 with a `Retry-After` header without starting a goroutine (AC-038, INV-001, POL-003). Goroutine count must not grow beyond the pool limit under any load.
- **Recovery path:** as soon as slots free up, new requests are accepted (AC-039).
- **Composition (ADR-0006):** pool middleware wraps the proxy core call; positioned in the middleware chain after rate-limit, before cache and provider call.

NFR-002: under 3× overload, 0 crashes — all excess requests are 503 (AC-043).
NFR-003: pprof goroutine baseline stable before/after load test (AC-044).
NFR-006: goroutines waiting on upstream never exceed pool limit (AC-047).

## Acceptance criteria

### FR-015 — Bounded worker pool

**AC-038**
- **Given:** worker pool is limited to N=10 slots, 10 requests are already in flight
- **When:** the 11th request arrives
- **Then:** gateway immediately returns 503 with Retry-After; goroutine count does not grow
- **Test:** `TestWorkerPool_Saturated_Returns503WithRetryAfter` (kind: happy)

**AC-039**
- **Given:** worker pool is saturated, then 5 slots become free
- **When:** new requests arrive
- **Then:** the next 5 requests are accepted without 503
- **Test:** `TestWorkerPool_RecoveryAfterSaturation` (kind: boundary)

### NFR-002 — Graceful degradation under overload

**AC-043**
- **Given:** load at 3× nominal RPS for 2 minutes
- **When:** load test runs
- **Then:** no panics occur, process uptime is 100%, all responses are 200/429/503; after load drops gateway accepts requests
- **Test:** `BenchGateway_Overload3x_NocrashGracefulDegradation` (kind: boundary)

### NFR-003 — No goroutine leaks

**AC-044**
- **Given:** baseline goroutine count recorded before the load test
- **When:** load test completes and load has subsided
- **Then:** goroutine count equals baseline (tolerance ±5 background goroutines)
- **Test:** `BenchGateway_GoroutineLeakCheck` (kind: boundary)

### NFR-006 — Upstream goroutines bounded

**AC-047**
- **Given:** worker pool limit = 50, load of 500 RPS
- **When:** load test runs for 1 minute
- **Then:** goroutine count on the upstream path never exceeds 50
- **Test:** `BenchGateway_GoroutineCountBounded` (kind: boundary)

## Architecture context

- **FR:** FR-015
- **NFR:** NFR-002, NFR-003, NFR-006
- **ADR:** ADR-0003, ADR-0006
- **Components:** COMP-010 Worker Pool / Backpressure
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
