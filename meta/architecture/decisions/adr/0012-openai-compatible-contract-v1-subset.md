# ADR-0012: OpenAI-compatible contract (v1 subset)

## Status

Accepted — 2026-06-26

## Context

`sluice` originally exposed a *simplified* edge contract on `POST /v1/chat/completions`:
a canonical request/response shape (`messages`, `content`, flat `usage`) that resembled
OpenAI but was not wire-identical. To be a genuine drop-in OpenAI-compatible LLM gateway
(Variant B — end-to-end OpenAI compatibility, with Ollama as the primary showcase backend),
the edge must speak the *real* OpenAI `/v1/chat/completions` wire format so unmodified
OpenAI SDKs and `curl` examples work against it without adaptation.

This changes the public contract, so it touches **ADR-0011** (contract-first OpenAPI: the
spec is the single source of truth and types are generated from it). It must also stay
faithful to **ADR-0009** (the Provider ACL): the OpenAI shape is an *edge adapter* concern;
only the canonical `provider.Request`/`Response`/`Chunk` cross the Provider boundary.

The scope must be deliberately bounded: full OpenAI surface (function calling, multimodal,
multiple endpoints) is out of proportion for a thin gateway demo. The decision records the
exact **v1 subset** that is in scope and the precise behaviour for fields outside it.

## Decision

The edge speaks the **real OpenAI `/v1/chat/completions` wire format**, scoped to a v1 subset:

1. **Endpoint scope.** Only `POST /v1/chat/completions`. `/v1/completions`, `/v1/embeddings`,
   and `/v1/models` are explicit non-goals (CON-007).

2. **Request fields — supported & forwarded upstream:** `model` (routing key + passthrough),
   `messages` (each `{role ∈ system|user|assistant, content: string}`), `stream`,
   `temperature`, `top_p`, `max_tokens`, `stop` (array of strings). Only these modeled
   fields are mapped onto the canonical `provider.Request` and forwarded.

3. **Request fields — accepted-but-IGNORED (liberal accept).** `seed`, `user`,
   `presence_penalty`, `frequency_penalty`, `logit_bias`, `response_format`, `n`,
   `logprobs` (and any other unknown field) are accepted and silently ignored — never a
   `400`. The edge schema sets **`additionalProperties: true`** so OpenAI SDKs that send
   extra fields do not get rejected. Ignored fields are *not* forwarded upstream.

4. **Request fields — NOT supported (documented non-goal, CON-008):** `tools` / `functions`
   / `tool_choice` (function calling), multimodal `content` (array/image parts), and `n > 1`.
   These are documented as non-goals; the gateway models only `content: string` and a
   single choice.

5. **Unary response — real OpenAI shape:**
   `{ id: "chatcmpl-…", object: "chat.completion", created, model, choices: [ { index: 0,
   message: { role: "assistant", content }, finish_reason } ], usage: { prompt_tokens,
   completion_tokens, total_tokens } }`. `id`, `created`, and `object` are **generated at the
   edge** (not passed through from upstream). Exactly **one choice** (`index: 0`).
   `system_fingerprint` is omitted.

6. **Streaming response — real OpenAI SSE chunks:**
   `{ object: "chat.completion.chunk", choices: [ { index: 0, delta: { … }, finish_reason } ] }`
   emitted as `data:` events, terminated by a literal `data: [DONE]`. For metering, the edge
   relies on the upstream adapter requesting `stream_options.include_usage=true` and parsing
   the final usage chunk (ADR-0013); if upstream omits usage, metering records 0/uncounted
   gracefully (the stream itself is unaffected).

7. **Errors — OpenAI error shape:** `{ error: { message, type, code } }` for the gateway's own
   `400 / 401 / 429 / 502 / 503` and for mapped upstream errors.

8. **Auth (edge).** A client `Authorization: Bearer <key>` is read as the rate-limit / metering
   key only — **no key validation** in v1 (still a non-goal, consistent with ADR-0001 and the
   existing CON set). Upstream auth is the adapter's concern (ADR-0013).

`api/openapi.yaml` is reworked to this contract (ADR-0011 discipline: spec is the source of
truth, types regenerated via `oapi-codegen`, handlers map the generated OpenAI DTOs ↔ the
canonical `provider.Request`/`Response`/`Chunk`). The canonical core, resilience, metering,
and observability layers are unchanged.

## Consequences

### Positive
- Genuine drop-in for OpenAI SDKs and tooling: the same client code that targets OpenAI works
  against sluice by changing only the base URL (and, for Ollama, dropping the key).
- Liberal-accept (`additionalProperties`) means real-world SDK payloads (which send `seed`,
  `user`, penalties, etc.) never 400 — a common compatibility failure mode is avoided.
- The contract is explicit and bounded: in/ignored/unsupported fields are enumerated, so
  clients and reviewers know exactly what the gateway honours.
- ADR-0009 stays intact: OpenAI specifics live in the edge adapter; the Provider boundary still
  carries only canonical types, so resilience/metering remain provider-agnostic.

### Negative
- The edge now owns OpenAI-specific concerns it did not before: generating `id`/`created`/
  `object`, shaping streaming chunks + `[DONE]`, and the OpenAI error envelope — more edge code
  and more contract tests.
- `additionalProperties: true` weakens strict validation of the request body: typos in unknown
  fields are silently ignored rather than reported. This is the intended trade-off for SDK
  compatibility.
- Streaming usage depends on the upstream honouring `stream_options.include_usage`; backends
  that omit it yield uncounted stream usage (graceful, but a metering gap).

### Neutral
- `n > 1`, function calling, and multimodal remain out of scope (CON-008); the response is
  always a single choice with string content.
- Health/readiness/metrics endpoints are unaffected by this contract change.
- SSE behaviour is still documented in the spec but exercised in code/contract tests, exactly
  as ADR-0011 already noted for streaming.

## Alternatives considered

- **Keep the simplified canonical edge shape (status quo).** Rejected: not a real drop-in;
  OpenAI SDKs cannot talk to it unmodified, defeating the purpose of Variant B.
- **Strict request validation (reject unknown fields with 400).** Rejected: real OpenAI SDKs
  routinely send fields outside our subset (`seed`, `user`, penalties); strict validation would
  400 legitimate clients. Liberal-accept is the compatible choice.
- **Pass upstream `id`/`created`/`object`/`system_fingerprint` straight through.** Rejected:
  the gateway is the OpenAI surface clients see; generating these at the edge keeps the contract
  stable and backend-independent (Ollama/vLLM/LM Studio shapes differ in these fields).
- **Implement the full OpenAI surface (tools, multimodal, embeddings, `/v1/models`).** Rejected
  for v1: disproportionate for a thin showcase gateway; recorded as explicit non-goals (CON-008).

## References

- DEC-012 (resolved by this ADR)
- ADR-0011 (contract-first OpenAPI — the edge schema changes here)
- ADR-0009 (Provider ACL — OpenAI specifics stay in the edge adapter)
- ADR-0001 (anonymous/ephemeral key — no key validation at the edge)
- ADR-0013 (real OpenAI-compatible upstream adapter — streaming usage, upstream auth)
- FR-017 (liberal OpenAI-compatible request acceptance)
- FR-018 (OpenAI-compatible unary response)
- FR-019 (OpenAI-compatible streaming + usage)
- FR-020 (OpenAI error-shape mapping)
- CON-007 (single endpoint scope), CON-008 (function-calling/multimodal/n>1 non-goals)
- CTX-001 (Proxy — owns the edge ↔ canonical mapping)

## History

- 2026-06-26: Created — OpenAI-compatible v1 subset wire contract at the edge; liberal-accept;
  edge-generated id/created/object; single choice; OpenAI streaming chunks + `[DONE]`; OpenAI
  error shape. Reworks the ADR-0011 OpenAPI spec; preserves the ADR-0009 Provider ACL.
