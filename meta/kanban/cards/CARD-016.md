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

—
