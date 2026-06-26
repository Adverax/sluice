# CARD-017: OpenAI-compatible edge (drop-in for OpenAI SDKs)

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 2.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/017-openai-compatible-edge
**Worktree:** ../sluice-card-017
**Source:** meta/architecture/handoff.md#6-increment-openai-compatibility
**Depends on:** CARD-016 ✓ (merged db94236)
**Review score:** —
**Started:** 2026-06-26T11:38:20Z
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Flip the gateway's public edge to the **real OpenAI** request/response/stream/error wire shape
(ADR-0012, under the ADR-0011 contract-first discipline) so an unmodified OpenAI SDK / `curl`
works against sluice. Canonical core + resilience/metering/observability unchanged — the OpenAI
shape lives only in the edge adapter (OpenAI DTO ↔ canonical `provider.Request`/`Response`/`Chunk`).

1. **Contract (`api/openapi.yaml`):** rework to the real OpenAI request/response/stream/error schema
   with **liberal accept** (`additionalProperties: true`). Modeled subset (`model`, `messages[{role,content}]`,
   `stream`, `temperature`, `top_p`, `max_tokens`, `stop`) is forwarded; unknown fields (`seed`, `user`,
   `presence_penalty`, `frequency_penalty`, `logit_bias`, `response_format`, `n`, `logprobs`) are
   **accepted-but-ignored — never 400**. Unsupported shapes (multimodal/array content, `n>1`) → OpenAI-shaped 400.
2. **Regenerate:** `go generate ./...` → `internal/api/` regenerated; keep `go generate` diff-clean (CI gate).
   Update the server DTO mapping (`internal/server`, `internal/proxy`) OpenAI DTO ↔ canonical.
3. **Unary response:** real `chat.completion` shape with **edge-generated** `id` (`chatcmpl-…`) / `created` /
   `object`; exactly one `choices[0]` with `message.role:"assistant"`; `system_fingerprint` omitted.
4. **Streaming:** emit `chat.completion.chunk` SSE events (`choices[0].delta`) terminated by literal `data: [DONE]`.
5. **Errors:** OpenAI envelope `{error:{message,type,code}}` for gateway 400/401/429/502/503 and mapped
   upstream errors (so OpenAI SDKs parse them).
6. **Docs + demo:** update the integrator API reference (`docs/role/integrator/**`) and README `curl`
   examples to the OpenAI shape. Add an **OPTIONAL** Ollama docker-compose profile + documented
   `GATEWAY_UPSTREAM_URL=http://…:11434/v1` usage; **keep the mock as the default `make up` upstream** so the
   demo stays fast. Verify an unmodified OpenAI SDK or the documented `curl` completes a unary + a streaming chat.

ADR-0012 (the contract), ADR-0011 (contract-first OpenAPI), ADR-0009 (Provider ACL preserved). Builds on
CARD-016 (the upstream already speaks OpenAI; this flips the edge to match).

## Acceptance criteria

**AC-053 — liberal OpenAI request accepted & forwarded**
- Given: a valid OpenAI request body with `model` and `messages[{role,content}]`
- When: API Client sends POST /v1/chat/completions
- Then: gateway accepts it and forwards model, messages, stream, temperature, top_p, max_tokens, stop as canonical fields
- Test: `TestEdge_OpenAIRequest_Accepted`

**AC-054 — unknown fields ignored, not 400**
- Given: an OpenAI request additionally carrying unknown fields (seed, user, presence_penalty, frequency_penalty, logit_bias, response_format, n, logprobs)
- When: API Client sends POST /v1/chat/completions
- Then: gateway returns 200 (does not 400); the unknown fields are ignored and not forwarded upstream
- Test: `TestEdge_UnknownFields_IgnoredNot400`

**AC-055 — unsupported content → OpenAI-shaped 400**
- Given: a request whose messages use an unsupported content shape (array/multimodal) or `n>1`
- When: API Client sends POST /v1/chat/completions
- Then: gateway returns an OpenAI-shaped 400 error (documented non-goal), without contacting the provider
- Test: `TestEdge_UnsupportedContent_Returns400`

**AC-056 — unary OpenAI response shape**
- Given: a non-streaming OpenAI request, provider responds 200 with content and usage
- When: API Client sends POST /v1/chat/completions
- Then: gateway returns 200 with `object:"chat.completion"`, an edge-generated `id` prefixed `chatcmpl-`, a `created` timestamp, a single `choices[0]` with `message.role:"assistant"` and `finish_reason`, and a `usage` object
- Test: `TestEdge_UnaryResponse_OpenAIShape`

**AC-057 — edge-generated fields**
- Given: a non-streaming OpenAI request
- When: gateway maps the canonical Response to the edge response
- Then: `id`/`created`/`object` are generated at the edge (not passed through from upstream); `system_fingerprint` is omitted
- Test: `TestEdge_UnaryResponse_EdgeGeneratedFields`

**AC-058 — streaming OpenAI chunks + [DONE]**
- Given: a request with `"stream":true`, provider streams deltas then a usage chunk
- When: API Client sends POST /v1/chat/completions
- Then: gateway returns Content-Type text/event-stream, forwards `object:"chat.completion.chunk"` events with `choices[0].delta`, and terminates with a literal `data: [DONE]`
- Test: `TestEdge_Streaming_OpenAIChunksAndDone`

**AC-060 — gateway error in OpenAI shape**
- Given: a request that triggers a gateway 429 (rate limit) or 503 (backpressure/breaker)
- When: API Client sends POST /v1/chat/completions
- Then: gateway returns the corresponding status with body `{error:{message,type,code}}` in the OpenAI error shape
- Test: `TestEdge_GatewayError_OpenAIShape`

**AC-061 — upstream error mapped to OpenAI shape**
- Given: the provider returns a non-2xx upstream error (e.g. 500) with retries exhausted
- When: API Client sends POST /v1/chat/completions
- Then: gateway returns 502 with body `{error:{message,type,code}}` mapped to the OpenAI error shape
- Test: `TestEdge_UpstreamError_MappedToOpenAIShape`

## Architecture context

- **FR:** FR-017, FR-018, FR-020, FR-019 (edge / SSE half)
- **CON:** CON-007 (endpoint scope), CON-008 (non-goals)
- **ADR:** ADR-0012, ADR-0011, ADR-0009
- **Components:** COMP-001 (HTTP Handler & Router), COMP-002 (Proxy Core)
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
