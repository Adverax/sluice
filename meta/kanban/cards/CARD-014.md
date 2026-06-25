# CARD-014: Streaming through the resilience seam

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 1.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/014-streaming-resilience
**Worktree:** —
**Source:** doc/requirements-audit.md (gap #3: streaming path bypasses pool/breaker + provider metrics/span)
**Depends on:** CARD-004, CARD-007, CARD-008, CARD-009, CARD-013
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

The audit found the `stream:true` path calls `provider.InferStream` DIRECTLY, bypassing the pool→retry→breaker
seam and the provider metric/span that the unary path gets. Close it by routing **stream initiation** through
a streaming-aware resilience seam (retry is intentionally N/A for a partially-sent stream — do NOT retry streams):

1. **Streaming resilience seam.** Add a `StreamFunc` analogous to the unary `InferFunc`, composed as
   **pool → breaker → provider.InferStream** (NO retry):
   - **Breaker guards initiation:** wrap the `InferStream` call so an OPEN breaker returns `ErrOpenState`
     immediately → the server fast-fails **503 + Retry-After BEFORE writing the SSE 200 header / any bytes**.
     A successful initiation counts as a breaker success; an initiation error counts as a failure (per-provider,
     same registry/key as unary). Mid-stream chunk errors need not feed the breaker in v1 (note the choice).
   - **Pool slot for streams:** acquire a worker-pool slot before starting the stream; saturation → 503 before
     any bytes (bounded concurrency must include streams — NFR-006). RELEASE the slot when the stream ends
     (channel closed / ctx cancelled / error) — wrap the returned `<-chan Chunk` so release happens exactly once
     on completion; no slot leak under cancellation.
2. **Instrumentation parity.** Record `provider_request_duration_seconds{provider}` for the stream (duration =
   stream lifetime, or time-to-completion) and create the provider OTel span around the stream, so streaming has
   the same metric/trace coverage as unary (gap #3).
3. **Wire `cmd/gateway`:** compose the `StreamFunc` (pool+breaker+instrumentation over the HTTP provider's
   InferStream) and inject it into the server (e.g. `server.WithStreamFunc`); `streamResponse` calls it instead
   of `provider.InferStream` directly. The 503 fast-fail must occur BEFORE `WriteHeader(200)` (so the client gets
   a real 503, not a 200 stream that immediately dies).

Keep retry OUT of the stream path (documented). Keep the existing SSE behavior (flush per chunk via
ResponseController, ctx cancel aborts upstream) intact (AC-002/008/009 stay green).

## Acceptance criteria

**AC-014a — open breaker fast-fails a stream before any bytes**
- Given: the per-provider breaker is OPEN
- When: a `stream:true` request arrives
- Then: gateway returns 503 + Retry-After with NO SSE bytes/200 header written; `InferStream` is NOT called
- Test: `TestStream_BreakerOpen_FastFail503`

**AC-014b — pool saturation rejects a stream**
- Given: the worker pool is saturated
- When: a `stream:true` request arrives
- Then: gateway returns 503 (no stream started); slot count never exceeds the limit (streams counted)
- Test: `TestStream_PoolSaturated_Returns503`

**AC-014c — streaming is instrumented (parity with unary)**
- Given: a successful stream
- When: it completes
- Then: `provider_request_duration_seconds{provider}` is observed and a provider OTel span is recorded for the stream
- Test: `TestStream_RecordsProviderMetricAndSpan`

**AC-014d — pool slot released on stream end (no leak)**
- Given: a stream that completes OR is cancelled mid-flight
- When: it ends
- Then: the acquired pool slot is released exactly once; repeated streams do not exhaust the pool
- Test: `TestStream_PoolSlotReleasedOnEndAndCancel`

## Architecture context

- **FR:** FR-001 (streaming), FR-007 (breaker), FR-015 (bounded concurrency)
- **NFR:** NFR-006 (upstream goroutines bounded incl. streams), NFR-007 (observability parity)
- **ADR:** ADR-0006 (composition), ADR-0002 (breaker), ADR-0003 (pool), ADR-0008 (observability)
- **Components:** COMP-002 Proxy Core (stream), COMP-010 Pool, COMP-011 Breaker, COMP-013/014 metrics/tracing
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
