# CARD-016: OpenAI-compatible upstream provider adapter (Ollama primary)

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 2d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/016-openai-upstream-adapter
**Worktree:** —
**Source:** meta/architecture/handoff.md#6-increment-openai-compatibility
**Depends on:** — (builds on CARD-013 HTTP provider + CARD-002 Provider interface, both merged)
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

A real HTTP provider adapter (behind the existing `Provider` port, ADR-0009) that speaks the
**real OpenAI `/v1/chat/completions` wire** to the configured backend — Ollama primary
(`http://localhost:11434/v1`, no key), OpenAI/vLLM/LM Studio via config. The gateway's own
edge stays simplified for now (valid intermediate state; CARD-017 flips it). Build on the
existing `internal/provider/httpprovider.go` (CARD-013) — replace its simplified wire shape
with the real OpenAI shape.

1. **OpenAI wire (request):** canonical `Request` → OpenAI request — `model`, `messages[{role,content}]`,
   `stream`, `temperature`, `top_p`, `max_tokens`, `stop` (array). POST to `<baseURL>/chat/completions`
   via `http.NewRequestWithContext` over the injected pooled client (ADR-0010), ctx-cancellable.
2. **OpenAI wire (response):** parse the REAL OpenAI unary response — `choices[0].message.content`,
   `choices[0].finish_reason`, `usage{prompt_tokens,completion_tokens,total_tokens}` → canonical `Response`.
3. **OpenAI wire (stream):** request the SSE variant with `stream_options:{include_usage:true}`; parse
   `data:` lines as `{choices:[{delta:{content},finish_reason}]}`, accumulate deltas → canonical `Chunk`s,
   stop on literal `data: [DONE]`. Parse the trailing usage chunk into the terminal `Chunk.Usage`.
   **Graceful 0/uncounted** if upstream omits usage (no error, no stream break).
4. **Upstream auth:** send `Authorization: Bearer <GATEWAY_UPSTREAM_API_KEY>` **only when the key is
   non-empty**; omit the header entirely when empty (Ollama needs none).
5. **Config + wiring (`cmd/gateway`, `internal/config`):** `GATEWAY_UPSTREAM_URL` (exists),
   `GATEWAY_UPSTREAM_API_KEY` (optional), `GATEWAY_UPSTREAM_MODEL` (model name, default doc `llama3.2`).
   Register the adapter for the configured model; `model` passed through as-is. The resilience seam
   (pool→retry→breaker / stream pool→breaker) wraps it unchanged.
6. **MockUpstream (`internal/provider/mockupstream.go`): UPDATE to emit the REAL OpenAI shape** — unary
   `chat.completion` + SSE `chat.completion.chunk` + `[DONE]` + a usage chunk — so unit + load tests
   exercise the real mapping. KEEP the interface-level mock `Provider` (fast unit tests). Mock stays the
   default `make up` upstream (fast demo). Update load/integration tests that asserted the old shape.

ADR-0009 (Provider ACL — no upstream wire types leak; only canonical Request/Response/Chunk cross the
boundary), ADR-0010 (injected client + explicit timeout), ADR-0013 (this decision). Edge unchanged.

## Acceptance criteria

**AC-062 — unary OpenAI mapping**
- Given: the adapter configured with `GATEWAY_UPSTREAM_URL` of an OpenAI-compatible backend + `GATEWAY_UPSTREAM_MODEL`
- When: the gateway calls `Infer` with a canonical Request
- Then: the adapter POSTs an OpenAI `/v1/chat/completions` request (model, messages, temperature, top_p, max_tokens, stop) and maps `choices[0].message.content` + `usage` back into the canonical Response
- Test: `TestUpstream_OpenAIAdapter_UnaryMapping`

**AC-063 — optional bearer auth**
- Given: `GATEWAY_UPSTREAM_API_KEY` is empty (Ollama)
- When: the adapter builds the upstream request
- Then: no `Authorization` header is sent; with a non-empty key it sends `Authorization: Bearer <key>`
- Test: `TestUpstream_OpenAIAdapter_BearerAuthOptional`

**AC-064 — mock upstream emits OpenAI shape**
- Given: the in-process mock upstream
- When: a unit or load test drives Infer/InferStream through the adapter
- Then: the mock emits the real OpenAI unary shape and SSE chunk shape (with `[DONE]` and a usage chunk), exercising the real mapping
- Test: `TestUpstream_MockUpstream_EmitsOpenAIShape`

**AC-059 — streaming missing-usage graceful zero**
- Given: a streaming request, the upstream omits the usage chunk (ignores `stream_options.include_usage`)
- When: the stream completes
- Then: the stream ends normally with `[DONE]`; metering records the usage event with zero/uncounted tokens, without error and without blocking the stream
- Test: `TestEdge_Streaming_MissingUsage_GracefulZero`

## Architecture context

- **FR:** FR-021, FR-019 (upstream / streaming-usage half)
- **CON:** CON-007, CON-008 (only modeled fields forwarded)
- **ADR:** ADR-0013, ADR-0009, ADR-0010
- **Components:** COMP-005 (Provider interface & adapter)
- **Trace:** meta/architecture/trace.yml

## Worktree notes

Implemented in worktree `sluice-card-016` on branch `card/016-openai-upstream-adapter`.

**OpenAI wire mapping (`internal/provider/httpprovider.go`).** Replaced the simplified wire
types with private `oaiRequest/oaiResponse/oaiStreamEvent/oaiErrorEnvelope` types (ADR-0009 ACL —
none cross the Provider boundary). Base URL now includes the `/v1` segment; the adapter POSTs to
`<baseURL>/chat/completions` via `http.NewRequestWithContext` over the injected pooled client.
- Request: `{model, messages:[{role,content}], stream, temperature?, max_tokens?}` (canonical
  fields only — CON-007/008; `top_p`/`stop` modeled in the wire struct, forwarded when the
  canonical Request grows them). `Request.Model` wins; `WithModel` is the fallback.
- Unary: parse `choices[0].message.content` + `finish_reason` + `usage{...}` → canonical Response;
  empty-`choices` handled defensively (empty completion, usage still flows).
- Stream: sets `stream_options:{include_usage:true}`; reads SSE `data:` lines, JSON-decodes each
  as `{choices:[{delta:{content},finish_reason}], usage?}`, emits a Chunk per non-empty
  `delta.content`, captures the trailing usage chunk (empty choices + usage) into the terminal
  `Done` Chunk.Usage, stops on literal `data: [DONE]`. GRACEFUL: missing usage chunk → terminal
  Done with zero usage (no error, no break); EOF without `[DONE]` also ends with a graceful Done.
  Drain/close-on-ctx-done discipline preserved (no goroutine leak).
- Errors: non-2xx → `*provider.StatusError` (5xx retryable / 4xx not unchanged); parses the OpenAI
  `{error:{message,type,code}}` envelope into the message when present.
- Auth: `Authorization: Bearer <key>` sent ONLY when the configured key is non-empty (Ollama omits).

**Mock upstream (`internal/provider/mockupstream.go`).** `MockUpstreamHandler` now emits the REAL
OpenAI shape: unary `chat.completion` (`id:"chatcmpl-mock"`, `object`, `created`, `choices[0].
message`, `usage`); SSE `chat.completion.chunk` deltas + a final chunk with `finish_reason` +
(when `include_usage` requested and `OmitStreamUsage` is false) a trailing usage chunk
(`choices:[]`,`usage`) + literal `data: [DONE]`. New `OmitStreamUsage` option models a backend
that ignores `include_usage`. Errors emit the OpenAI envelope. Latency/FailStatus/StreamChunks
honoured. Interface-level `Mock` (`mock.go`) untouched.

**Config + wiring (`internal/config/config.go`, `cmd/gateway/main.go`).** Added
`GATEWAY_UPSTREAM_API_KEY` (optional) and `GATEWAY_UPSTREAM_MODEL` (default `mock` for the
in-process mock so the default boot + tests are unchanged; `llama3.2` when an external
`GATEWAY_UPSTREAM_URL` is set). The adapter is registered for the configured model with
`WithAPIKey`+`WithModel`; breaker-state seeded for that model. The in-process mock upstream base
URL now carries `/v1` (`http://host:port/v1`). Mock stays the default when URL is empty
(`make up` fast). `TestConfig_AllBoundariesHaveTimeouts` green.

**AC → test mapping (all green under `go test -race ./...`):**
- AC-062 `TestUpstream_OpenAIAdapter_UnaryMapping` (+ `_RequestModelOverride` for model passthrough)
- AC-063 `TestUpstream_OpenAIAdapter_BearerAuthOptional`
- AC-064 `TestUpstream_MockUpstream_EmitsOpenAIShape`
- AC-059 `TestEdge_Streaming_MissingUsage_GracefulZero` (zero usage, no error, no goroutine leak)
- CARD-013 carryover (pooling/reuse/ctx-cancel/status) adapted to the new wire and kept green.

**Verification:** `go build ./...` clean; `go test -race ./...` green (incl. `-tags integration`);
`go vet ./...` clean; `go generate ./...` DIFF-CLEAN (no api.gen.go/openapi.yaml change — edge is
CARD-017); `go mod tidy` no-op. Files touched: cmd/gateway/main.go, internal/config/config.go,
internal/provider/{httpprovider,mockupstream}.go + their tests. Edge / api / server DTOs untouched.
