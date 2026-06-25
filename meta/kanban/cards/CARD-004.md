# CARD-004: SSE streaming & context cancellation

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 2d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/004-sse-streaming-context-cancellation
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-2
**Depends on:** CARD-003
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Extend `internal/proxy/**.go` with the streaming path in COMP-002 Proxy Core.

- **Streaming path:** when `"stream":true` is present in the request body, call `Provider.InferStream(ctx, req)` to get a `<-chan Chunk`. Iterate the channel, write each chunk as an SSE event, and flush via `http.Flusher` after each write. Set `Content-Type: text/event-stream` on the response (AC-002).
- **Context propagation end-to-end:** the `http.Request` context flows unmodified into `Provider.InferStream`; any cancellation (deadline, client disconnect) propagates to the upstream call.
- **Client disconnect cancellation:** when the client closes the connection (detected via `r.Context().Done()`), the goroutine consuming the chunk channel exits, causing the `ctx` passed to `InferStream` to be cancelled, which aborts the upstream HTTP call within 50ms (AC-008, INV-002, POL-006).
- **Streaming client cancel:** same mechanism applies to an active SSE stream — gateway stops forwarding chunks and upstream is cancelled (AC-009).

ADR-0009: streaming path uses `Provider.InferStream` through the same interface; no provider-specific types cross the boundary. ADR-0006: composition order is unchanged (rate-limit → pool → cache-bypass → proxy core streaming path).

## Acceptance criteria

### FR-001 — Proxy (streaming path)

**AC-002**
- **Given:** valid request with `"stream":true`, provider is available
- **When:** API Client sends POST /v1/chat/completions
- **Then:** gateway returns 200 with `Content-Type: text/event-stream` and forwards SSE chunks as they arrive
- **Test:** `TestProxy_HappyPath_Streaming` (kind: happy)

### FR-003 — Context cancellation

**AC-008**
- **Given:** a request to the provider is in flight, mock provider has 500ms latency
- **When:** client closes the connection 100ms after sending the request
- **Then:** upstream HTTP call terminates with context.Canceled within 50ms of the connection being closed
- **Test:** `TestProxy_ClientCancel_AbortsUpstream` (kind: happy)

**AC-009**
- **Given:** SSE stream is active, provider is sending chunks
- **When:** client closes the connection
- **Then:** gateway stops forwarding chunks and the upstream call is cancelled
- **Test:** `TestProxy_StreamingClientCancel_AbortsUpstream` (kind: happy)

## Architecture context

- **FR:** FR-001 (streaming AC), FR-003
- **NFR:** —
- **ADR:** ADR-0009, ADR-0006
- **Components:** COMP-002 Proxy Core (InferStream path)
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
