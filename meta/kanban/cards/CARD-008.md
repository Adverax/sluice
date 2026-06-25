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

Implemented COMP-010 in `internal/pool/`.

**Files**
- `internal/pool/pool.go` — `Pool` (buffered-channel semaphore), `New(size, retryAfter)`, `(*Pool).Guard(next server.InferFunc) server.InferFunc`, package-level `Guard(size, retryAfter, next)` convenience, and `ErrPoolSaturated` typed sentinel.
- `internal/pool/pool_test.go` — unit tests (race-safe, channel-gated, no sleeps).
- `internal/pool/bench_test.go` — small-scale NFR LOAD proxies for CARD-011.
- `cmd/gateway/main.go` — wires `pool.Guard(cfg.WorkerPoolSize, cfg.Breaker.RetryAfter, composer.InferFunc())` into `server.WithInferFunc`.

**Composition (ADR-0006), order:** pool acquire (reject-before-work) → retry → breaker → provider. The guard preserves the `server.InferFunc` signature, so CARD-005's rate-limit middleware sits OUTSIDE/above this layer unchanged.

**Backpressure:** non-blocking acquire via `select { case sem<-{}: ; default: }`. When full, returns `&saturatedError` IMMEDIATELY — no goroutine started, no blocking (AC-038, INV-001). Slot released via `defer` on every return (success/error/panic) so freed slots accept new work instantly (AC-039). Concurrency can never exceed `cap(sem)` = `GATEWAY_WORKER_POOL_SIZE` (default 100, ADR-0003).

**503 mapping:** `saturatedError` `Unwrap()`s to `ErrPoolSaturated` (direct `errors.Is`) and its `Is` matches `server.ErrServiceUnavailable`, plus it exposes `RetryAfter()`. The server's existing 503/Retry-After path (the same one used by the resilience `Unavailable`) maps it to HTTP 503 + `Retry-After` with NO new server code and NO string-matching.

**AC → test mapping**
- AC-038 → `TestWorkerPool_Saturated_Returns503WithRetryAfter` (N=10 held, 11th fast-fails → 503+Retry-After; asserts immediate return + no goroutine growth).
- AC-039 → `TestWorkerPool_RecoveryAfterSaturation` (saturate, free 5, next 5 accepted).
- NFR-006/AC-047 (unit) → `TestWorkerPool_NeverExceedsLimit` (64 callers vs limit 8: max observed concurrency == limit exactly, excess gets sentinel).
- Extra: `TestPool_New_PanicsOnNonPositiveSize`, `TestPool_Guard_ReleasesOnError` (slot not leaked on error).
- AC-043 → `BenchGateway_Overload3x_NocrashGracefulDegradation` (SMALL-SCALE proxy).
- AC-044 → `BenchGateway_GoroutineLeakCheck` (SMALL-SCALE proxy, baseline ±5).
- AC-047 → `BenchGateway_GoroutineCountBounded` (SMALL-SCALE proxy).

The three `BenchGateway_*` functions are in-package PROXIES that assert the same invariants quickly (~thousands of iters). The FULL-scale soak (3×/500 RPS, 2 min, pprof baseline) is CARD-011's k6/load harness against the real gateway — noted in `bench_test.go`.

**Status:** `go build ./...` OK; `go test -race ./...` all green; `go generate ./...` diff-clean (internal/api untouched); `go mod tidy` no change (pool uses only stdlib + internal pkgs — buffered-channel semaphore, no x/sync).
