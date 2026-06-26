# CARD-017: OpenAI-compatible edge (drop-in for OpenAI SDKs)

**Status:** done
**Priority:** P1
**Category:** feature
**Estimate:** 2.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/017-openai-compatible-edge
**Worktree:** —
**Source:** meta/architecture/handoff.md#6-increment-openai-compatibility
**Depends on:** CARD-016 ✓ (merged db94236)
**Review score:** 9.5 (2 cycles; cycle-1 important: scalar `stop` 400 → fixed; AC-053..061 ✓; integration live-green)
**Started:** 2026-06-26T11:38:20Z
**Closed:** 2026-06-26T12:21:28Z
**Actual:** 0.1d
**Merge commit:** 0eb8c93
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

Implemented on branch `card/017-openai-compatible-edge`.

**Contract (`api/openapi.yaml`, regenerated diff-clean).** Reworked
`POST /v1/chat/completions` to the real OpenAI wire shape: request
`{model, messages[{role,content}], stream, temperature, top_p, max_tokens, stop}`
with `additionalProperties: true` on BOTH the request and message schemas
(liberal-accept); unary `chat.completion` response
`{id, object, created, model, choices:[{index, message:{role,content},
finish_reason}], usage}`; streaming documented as `text/event-stream`; OpenAI
error envelope `{error:{message,type,code}}` (+ added 401). `go generate ./...`
regenerated `internal/api/api.gen.go` and is **idempotent / diff-clean**
(verified by re-running and diffing).

**DTO mapping (`internal/server/edge.go` new + `server.go`).**
- Request→canonical: forwards model/messages/stream/temperature/top_p/max_tokens/
  stop; unknown fields ignored (not forwarded); `n>1` → OpenAI-shaped 400 without
  contacting the provider; array/multimodal content rejected by the validator.
- canonical Response→unary: edge-generated `id` (`chatcmpl-`+crypto/rand hex),
  `created` (time.Now().Unix()), `object` "chat.completion"; exactly one
  `choices[0]` role "assistant"; `system_fingerprint` omitted.
- canonical Chunk→stream: `chat.completion.chunk` SSE events with stable
  id/created/model across the stream, a final empty-delta+finish_reason chunk,
  then literal `data: [DONE]`. Per-chunk flush + ctx-cancel + CARD-014 resilience
  seam unchanged.
- Errors: gateway 400/401/429/500/502/503 + mapped upstream errors → OpenAI
  envelope. Updated the rate-limit and recover middleware bodies to the envelope
  too (AC-060). Retry-After headers preserved.

**Canonical extension.** Added `TopP *float64` and `Stop []string` to
`provider.Request` and wired them edge→canonical→`HTTPProvider.toOAIRequest`, so
`top_p`/`stop` now actually forward upstream (CARD-016 fields were unpopulated).

**Tests.** Updated all edge/server/api/resilience tests asserting the old flat
shape. Added `internal/server/openai_edge_test.go` with AC-053..061:
`TestEdge_OpenAIRequest_Accepted` (053), `…_UnknownFields_IgnoredNot400` (054),
`…_UnsupportedContent_Returns400` (055, n>1 + array content), `…_UnaryResponse_OpenAIShape`
(056), `…_UnaryResponse_EdgeGeneratedFields` (057, unique ids + no
system_fingerprint), `…_Streaming_OpenAIChunksAndDone` (058), `…_GatewayError_OpenAIShape`
(060), `…_UpstreamError_MappedToOpenAIShape` (061).

**Docs + demo.** README curl/SDK examples + integrator docs (chat-completions,
streaming, errors-and-resilience, api-reference, getting-started, rate-limits)
reshaped to OpenAI; added an optional `ollama` docker-compose profile
(`--profile ollama`, NOT default — mock stays the default `make up` upstream) +
documented `GATEWAY_UPSTREAM_URL=http://ollama:11434/v1` /
`GATEWAY_UPSTREAM_MODEL=llama3.2` in README + config README.

**Verification.** `go build ./...` ✓; `go test -race ./...` ✓ (19 pkgs);
`go test -tags=integration -race -p 1 ./internal/integration/` ✓ (Docker /
testcontainers); `go generate ./...` diff-clean ✓; `go vet` + gofmt clean;
`go mod tidy` no-op; golangci-lint v2 (v2.1.6 via `go run`, since the local
binary is v1 / incompatible with the v2 config) reports **0 issues** repo-wide.
