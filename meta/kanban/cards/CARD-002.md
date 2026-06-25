# CARD-002: Provider interface & mock

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 1.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/002-provider-interface-mock
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-1
**Depends on:** CARD-001
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Define the Provider abstraction (COMP-005 Provider Interface & Mock Adapter) under `internal/provider/**.go`.

- `Provider` Go interface with exactly **two** methods:
  - `Infer(ctx context.Context, req Request) (Response, error)` — unary (non-streaming)
  - `InferStream(ctx context.Context, req Request) (<-chan Chunk, error)` — streaming
- Canonical gateway types: `Request`, `Response`, `Chunk` — these are the shapes used everywhere inside the gateway; adapter code normalises real provider wire-formats into them (anti-corruption per ADR-0009).
- Configurable mock provider: controllable per-call latency and error rate (injected via struct fields or functional options); used by all proxy tests as the stand-in for a real LLM provider.
- The adapter normalises upstream provider responses (HTTP JSON / SSE) into `Response`/`Chunk` on the way in, keeping the rest of the code provider-agnostic.

ADR-0009: the Provider interface is the single anti-corruption layer between the gateway core and the external LLM API (EXT-001). No provider-specific types cross this boundary.

Note: acceptance criteria for this component are exercised by the proxy tests in CARD-003 and CARD-004. This card has no standalone test suite — coverage is via proxy integration.

## Acceptance criteria

The Provider interface supports both FR-001 paths (unary and streaming) and FR-003 (context propagation). Verbatim ACs are tested through the proxy tests authored in CARD-003/CARD-004. Listed here for traceability:

### FR-001 — Proxy (non-streaming path via Provider.Infer)

**AC-001**
- **Given:** valid request with model and messages, provider is available and responds 200
- **When:** API Client sends POST /v1/chat/completions
- **Then:** gateway returns 200 with the provider response body within < upstream latency + 20ms overhead
- **Test:** `TestProxy_HappyPath_NonStreaming` (kind: happy) — authored in CARD-003

**AC-003**
- **Given:** provider returns 500
- **When:** API Client sends POST /v1/chat/completions (retries exhausted)
- **Then:** gateway returns 502 with a JSON error body
- **Test:** `TestProxy_ProviderError_Returns502` (kind: error) — authored in CARD-003

### FR-001 — Proxy (streaming path via Provider.InferStream)

**AC-002**
- **Given:** valid request with `"stream":true`, provider is available
- **When:** API Client sends POST /v1/chat/completions
- **Then:** gateway returns 200 with `Content-Type: text/event-stream` and forwards SSE chunks as they arrive
- **Test:** `TestProxy_HappyPath_Streaming` (kind: happy) — authored in CARD-004

### FR-003 — Context cancellation

**AC-008**
- **Given:** a request to the provider is in flight, mock provider has 500ms latency
- **When:** client closes the connection 100ms after sending the request
- **Then:** upstream HTTP call terminates with context.Canceled within 50ms of the connection being closed
- **Test:** `TestProxy_ClientCancel_AbortsUpstream` (kind: happy) — authored in CARD-004

**AC-009**
- **Given:** SSE stream is active, provider is sending chunks
- **When:** client closes the connection
- **Then:** gateway stops forwarding chunks and the upstream call is cancelled
- **Test:** `TestProxy_StreamingClientCancel_AbortsUpstream` (kind: happy) — authored in CARD-004

## Architecture context

- **FR:** FR-001 (both paths), FR-002, FR-003
- **NFR:** —
- **ADR:** ADR-0009
- **Components:** COMP-005 Provider Interface & Mock Adapter
- **Trace:** meta/architecture/trace.yml

## Worktree notes

### 2026-06-25 — implemented (golang-pro)

COMP-005 Provider interface + Mock adapter landed under `internal/provider/`.

**Files**
- `internal/provider/provider.go` — package doc, `Provider` interface (exactly `Infer` + `InferStream`), canonical types `Request`, `Response`, `Chunk`, plus `Message`/`Role`/`Usage`. Includes the deferred "writing a real adapter" ACL contract note (ADR-0009); no real adapter implemented.
- `internal/provider/mock.go` — configurable `Mock` (struct fields + functional options `New/WithResponse/WithLatency/WithError/WithStreamChunks`), `ErrMockFailure` sentinel, ctx-honouring `sleepCtx` helper, even `splitContent` chunker. No global state. Deterministic `ErrorRate` gate (>=1 fail, <=0 ok) so tests are reproducible under -race.
- `internal/provider/mock_test.go` — unit tests + compile-time `var _ Provider = (*Mock)(nil)`.

**Design notes**
- `Infer` and `InferStream` both honour ctx (FR-003): `sleepCtx` selects on `ctx.Done()`; the stream producer goroutine selects on `ctx.Done()` at every send and always closes the channel via `defer close(out)` (no goroutine leak).
- Canonical `Usage` (prompt/completion/total) lives on `Response` and on the terminal `Chunk` so CARD-010 metering reads canonical fields only.
- `InferStream` reports a configured failure as a synchronous init error (nil channel); per-chunk transport failures would arrive as a terminal `Chunk.Err`.

**Test coverage**
- `TestMockProvider_Infer_ReturnsConfiguredResponse` — happy unary, usage populated.
- `TestMockProvider_Infer_ErrorRate` — table-driven (default sentinel, custom err, rate>=1, rate<=0).
- `TestMockProvider_Infer_ContextCancelled` — cancel mid-latency returns `context.Canceled` promptly (<1s of a 2s latency).
- `TestMockProvider_InferStream_EmitsAndClosesChannel` — N content chunks reassemble Content, terminal Done chunk carries Usage, channel closes.
- `TestMockProvider_InferStream_InitErrorOnFailure` — configured failure → init error, nil channel.
- `TestMockProvider_InferStream_ContextCancelled_StopsAndCloses` — cancel mid-stream stops emission, channel closes within 1s (proves no goroutine leak).

**Result:** `go vet ./...`, `go build ./...`, `golangci-lint run ./internal/provider/...`, and `go test -race ./...` all green.
