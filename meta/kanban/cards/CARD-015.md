# CARD-015: Conformance tweaks (token-bucket, buffer-size metric, drained/flushed log)

**Status:** ready
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
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
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

—
