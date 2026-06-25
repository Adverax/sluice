# CARD-015: Conformance tweaks (token-bucket, buffer-size metric, drained/flushed log)

**Status:** done
**Priority:** P2
**Category:** tech-debt
**Estimate:** 1d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/015-conformance-tweaks
**Worktree:** —
**Source:** doc/requirements-audit.md (gap #5: minor spec-conformance items)
**Depends on:** CARD-005, CARD-010
**Review score:** 9.0 (1 cycle; 0 critical/important; AC-015a–c ✓; integration live-green)
**Started:** 2026-06-25T14:24:09Z
**Closed:** 2026-06-25T14:46:27Z
**Actual:** 0.1d
**Merge commit:** a24b58d
**Blocked by:** —

## What to implement

Three small, independent spec-conformance items the audit flagged (gap #5):

1. **Distributed rate limit → Redis token bucket** (`internal/ratelimit/redisrepo.go`). The spec says
   "token bucket … + Redis"; the current distributed tier is a fixed-window counter. Replace the Lua with a
   **token-bucket** algorithm: per key store `{tokens, last_refill_ts}`; on each call refill
   `tokens = min(burst, tokens + elapsed * rate)`, allow iff `tokens >= 1` (decrement), set a TTL.
   Keep the `RateLimitRepository` interface unchanged (the middleware shouldn't care). The local tier
   stays `x/time/rate`. Update unit tests (fake) + the AC-013 distributed semantics (still: no more than
   ~limit allowed per window across instances) + the integration test (real Redis) accordingly.
2. **`metering_buffer_size` gauge** (`internal/metrics/`, `internal/metering/`). The spec §observability lists
   it; only `metering_events_dropped_total` exists today. Add a gauge reporting current Usage-buffer occupancy
   (channel length / capacity). Wire it via the injected metrics recorder (metering must NOT import prometheus —
   keep the ADR-0008 boundary): e.g. the buffer/worker updates the gauge on enqueue/flush, or expose
   `Buffer.Len()` and have the worker set it periodically. Expose at `/metrics`. Add a test.
3. **Shutdown log: "drained N requests" + "flushed M usage events"** (`internal/lifecycle/`, `internal/metering/`).
   The spec wants the graceful-shutdown line to report both the drained in-flight requests AND the flushed
   metering events. The metering worker's `Close`/flush should return (or record) the count of events flushed on
   shutdown; the lifecycle shutdown log (or the OnShutdown hook) logs that count alongside "drained N requests".
   Update/extend the shutdown test to assert the flushed-count appears.

All three are independent; keep them cohesive but small. Do not regress existing rate-limit / metering / shutdown ACs.

## Acceptance criteria

**AC-015a — distributed token bucket**
- Given: two gateway instances sharing one Redis, a per-key token-bucket limit
- When: a burst arrives across both instances
- Then: the shared limit is enforced as a token bucket (burst up to `burst`, then `rate`-limited); excess → 429
- Test: `TestRateLimit_DistributedTokenBucket` (fake + the existing distributed test adapted) + integration (real Redis)

**AC-015b — metering_buffer_size exposed**
- Given: the gateway with usage events buffered
- When: GET /metrics is scraped
- Then: `metering_buffer_size` gauge is present and reflects buffer occupancy
- Test: `TestMetrics_MeteringBufferSizePresent`

**AC-015c — shutdown logs drained + flushed**
- Given: in-flight requests and buffered usage events at SIGTERM
- When: graceful shutdown completes
- Then: the log contains both "drained N requests" and the count of flushed usage events
- Test: `TestGracefulShutdown_LogsDrainedAndFlushed`

## Architecture context

- **FR:** FR-004 (rate limit), FR-014 (metering), FR-012 (graceful shutdown)
- **NFR:** NFR-007 (observability completeness)
- **ADR:** ADR-0010 (repo ACL), ADR-0008 (injected metrics), ADR-0007 (metering), ADR-0001 (rate limit)
- **Components:** COMP-009 RateLimitRepository, COMP-013 Metrics, COMP-016/017 Metering, COMP-006 Lifecycle
- **Trace:** meta/architecture/trace.yml

## Worktree notes

Implemented all three conformance items on branch `card/015-conformance-tweaks`.

**Item 1 — Redis token bucket (`internal/ratelimit/redisrepo.go`).** Replaced the
fixed-window `INCR+PEXPIRE+PTTL` Lua with an atomic token-bucket script: per key it
stores a hash `{tokens, ts}`, refills `tokens = min(burst, tokens + elapsed_s·rate)`
against a `now_ms` passed from Go (deterministic/testable, NTP-synced-clock trade-off
noted in the script doc), allows iff `tokens >= 1` (decrement), persists with a TTL of
5× the window, and returns `{allowed, retry_after_ms}`. Read-refill-decrement-persist is
one Lua call → no read-modify-write race (ADR-0010). Interface unchanged: `rate =
limit/window`; `burst` injected via the new `WithBurst` option (reuses
`GATEWAY_RATELIMIT_BURST`, wired in `cmd/gateway`), defaulting to `limit`. Clock injected
via `WithRedisClock` (named distinctly from the Registry's `WithClock`). Tests: the fake
scripter now emulates the token-bucket math exactly; added `TestRateLimit_DistributedTokenBucket`
(two repos sharing one fake store → no more than `burst` immediate, then steady-state
rate, capped), `TestRedisRepository_TokenBucket`, `TestRedisRepository_SteadyStateRate`.
The integration test `TestIntegration_RateLimitRedisDistributed` was rewritten to the
token bucket against REAL Redis using a shared injected clock (no real sleeps): phase 1
asserts exactly `burst` admitted across two instances, phase 2 advances the clock one
window and asserts `0 < allowed <= burst`. Integration run: **PASS** (0.83s, real Redis
via testcontainers).

**Item 2 — `metering_buffer_size` gauge.** Added the gauge to `internal/metrics` on the
injected registry (`promauto.With(reg)`); extended the `Recorder` interface (and
`NopRecorder`) with `SetMeteringBufferSize(int)`. metering does NOT import Prometheus
(verified via `go list -deps`): added a narrow `BufferSizeRecorder` port + `NopBufferSizeRecorder`;
the worker holds the `*Buffer` and publishes `buf.Len()` each loop tick (and after every
dequeue/flush) via `WithBufferSizeRecorder(met)` (wired in `cmd/gateway`). Exposed at
`/metrics` (same registry). Test: `TestMetrics_MeteringBufferSizePresent`.

**Item 3 — shutdown logs drained + flushed.** The worker now counts events successfully
persisted during the stop-path drain (`flush`/`drain` return counts) and exposes
`FlushedOnShutdown() int`. Lifecycle gained `WithFlushedCountFn(func() int)`, read AFTER
the shutdown hooks run, and logs `"shutdown complete: drained N requests, flushed M usage
events"` alongside the existing (intact) `"drained N requests"` and forced-shutdown lines.
Wired in `cmd/gateway` to `meteringWorker.FlushedOnShutdown`. Tests:
`TestGracefulShutdown_LogsDrainedAndFlushed` (lifecycle) + `FlushedOnShutdown` assertion
in `TestGracefulShutdown_FlushesMetering` (metering).

**Gates:** `go build ./...` ✓ · `go test -race ./...` ✓ (all packages green) ·
`go test -tags=integration -race -p 1 ./internal/integration/` ✓ · `go generate ./...`
diff-clean (no `api.gen.go`/`openapi.yaml` changes) ✓ · `golangci-lint run` clean ✓ ·
`go mod tidy` no-op ✓.
