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

### CARD-004 implementation (card/004-sse-streaming)

**Streaming wiring (Visit seam).** `CreateChatCompletion` reuses the unary
validate→route→map pipeline, then branches on `req.Stream`. For a stream it
returns a custom `streamResponse` strict-response object (instead of a buffered
`CreateChatCompletion200JSONResponse`). Its
`VisitCreateChatCompletionResponse(w http.ResponseWriter)` — the same generated
response-visitor seam CARD-009 used for `/metrics` — takes over the raw writer:
sets `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
`Connection: keep-alive`, writes 200, then forwards each canonical `Chunk` as an
SSE `data: <json>\n\n` event and ends with `data: [DONE]`. Chunks are marshalled
to a provider-agnostic `streamChunk` wire shape (content + canonical usage) — no
provider type crosses the boundary (ADR-0009).

**Flush through the Unwrap chain.** Flushing uses
`http.NewResponseController(w).Flush()` after each write (headers + every event),
NOT a raw `w.(http.Flusher)` assertion — the metrics/tracing ResponseWriter
wrappers implement `Unwrap() http.ResponseWriter`, and the controller traverses
that chain to reach the real flusher.

**Ctx cancellation aborts upstream (no leak).** The per-request context
(`r.Context()`, received by the handler as `ctx`) is captured in `streamResponse`
and passed straight into `provider.InferStream(ctx, req)`. The Visit loop
`select`s on `ctx.Done()` vs the chunk channel; on client disconnect/deadline it
stops forwarding and returns. Because that same ctx was handed to InferStream,
returning cancels the upstream, so the Mock's emit goroutine observes
`ctx.Done()`, stops, and closes its channel — proven by
`TestProxy_StreamingClientCancel_AbortsUpstream` (NumGoroutine before/after with
tolerance + a `stopped` atomic signal from the stream double) and
`TestProxy_ClientCancel_AbortsUpstream` (asserts the goroutine stopped and the
observed upstream ctx error is `context.Canceled`).

**AC → test mapping.**
- AC-002 → `TestProxy_HappyPath_Streaming` (200, text/event-stream, ≥2 `data:`
  events, terminal `[DONE]`).
- AC-008 → `TestProxy_ClientCancel_AbortsUpstream` (500ms latency, cancel ~100ms
  in, handler returns <150ms, upstream ctx == context.Canceled).
- AC-009 → `TestProxy_StreamingClientCancel_AbortsUpstream` (active stream,
  mid-stream cancel, forwarding stops, no goroutine leak).
Existing unary tests stay green (streaming is an added branch).

**FOLLOW-UP — resilience for streaming (deferred, not an AC).** The streaming
initiation calls the router-resolved provider's `InferStream` directly; it is NOT
guarded by the circuit breaker (open-breaker fast-fail before stream start) nor
does it take a pool slot. Retry is intentionally N/A for a stream (cannot retry a
partially-sent response). The UNARY path retains full resilience
(pool→retry→breaker→Infer via the injected InferFunc). Wiring breaker-guard on
stream initiation + pool slot acquisition for the streaming path is a documented
follow-up.

**Validation.** `go build ./...`, `go vet ./...`, `go test -race ./...` all green;
`go generate ./...` diff-clean (no change to internal/api/api.gen.go or
api/openapi.yaml); `go mod tidy` no-op.
