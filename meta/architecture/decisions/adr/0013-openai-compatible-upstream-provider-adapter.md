# ADR-0013: Real OpenAI-compatible upstream provider adapter (Ollama as primary backend)

**Status:** Accepted
**Date:** 2026-06-26
**Deciders:** @roman.miakotin
**Revised:** —

## Context

ADR-0009 defined the single `Provider` interface as the anti-corruption layer (ACL) between the
gateway core (CTX-001) and an external LLM provider (EXT-001), and explicitly anticipated real
adapters. ADR-0010 established that the upstream `*http.Client` is injected (pooling + total
timeout owned by `cmd/gateway`). The current code ships `HTTPProvider` (`httpprovider.go`)
talking to an in-process **mock upstream** over a *simplified* OpenAI-ish wire shape, plus the
interface-level `Mock`.

For Variant B (end-to-end OpenAI compatibility), the gateway must front a **real** model. The
upstream adapter must speak the **real OpenAI `/v1/chat/completions` wire format** so it can talk
to **Ollama** (primary showcase, OpenAI-compatible API at `…:11434/v1`, no API key) and, by
configuration alone, to **OpenAI / vLLM / LM Studio**. This is the upstream counterpart of the
edge contract in ADR-0012.

Decisions needed: the adapter's wire shape and request mapping; how the streaming usage is
obtained for metering; how upstream auth is handled (Ollama needs none, OpenAI needs a key);
how the adapter is configured and routed; and what happens to the existing mock(s).

## Decision

We ship a **real HTTP provider adapter that speaks the OpenAI wire format**, configured by base
URL + optional bearer key + model:

1. **Wire shape = real OpenAI.** The adapter maps the canonical `provider.Request` onto an OpenAI
   `/v1/chat/completions` request (`model`, `messages[{role,content}]`, `stream`, `temperature`,
   `top_p`, `max_tokens`, `stop`) and parses the real OpenAI response: unary
   `choices[0].message.content` + `usage` → canonical `Response`; streaming `choices[0].delta` +
   `[DONE]` → canonical `Chunk`s. No upstream wire type crosses the Provider boundary (ADR-0009).

2. **Streaming usage for metering.** On the streaming path the adapter sets
   `stream_options.include_usage = true` and parses the trailing usage chunk into the terminal
   canonical `Chunk.Usage`. If the upstream omits usage (a backend that ignores
   `include_usage`), the adapter completes the stream normally and reports zero/uncounted usage —
   metering records it gracefully (no error, no stream interruption).

3. **Upstream auth.** The adapter sends `Authorization: Bearer <GATEWAY_UPSTREAM_API_KEY>` only
   when that key is **non-empty**; when empty the header is omitted entirely (Ollama needs no
   key). The key is upstream-only and never confused with the client edge key (ADR-0012 §8).

4. **Configuration & routing (v1).** A single configured upstream:
   `GATEWAY_UPSTREAM_URL` (e.g. Ollama `http://localhost:11434/v1`), optional
   `GATEWAY_UPSTREAM_API_KEY`, and `GATEWAY_UPSTREAM_MODEL` (demo/routing model, default doc
   `llama3.2`). The adapter is registered for the configured model; the `model` field is passed
   through as-is. Multi-provider routing by model prefix remains a future non-goal.

5. **Mocks retained.** The in-process **mock upstream** (`mockupstream.go`) is updated to emit the
   **real OpenAI shape** (unary + SSE chunks + `[DONE]` + usage chunk) so unit and load tests
   exercise the real mapping. The interface-level `Mock` (configurable latency/error) is kept for
   tests that need a `Provider` double without HTTP. The mock stays the default `make up` upstream
   so the demo remains fast and dependency-free.

The canonical types and the Provider interface (ADR-0009) are unchanged; this adapter is a new
implementation behind the same port.

## Alternatives considered

### http_passthrough to a real backend

Forward the client's HTTP body straight to the upstream URL with no Go interface. Rejected for the
same reason as in ADR-0009: it collapses the ACL, leaks the upstream wire/auth/error shape into
the core, and breaks the per-provider circuit breaker (FR-007). It would also couple the edge
contract (ADR-0012, with edge-generated `id`/`created`) to whatever the backend returns.

### Backend-specific adapters (one per Ollama/OpenAI/vLLM/LM Studio)

Rejected for v1: Ollama, vLLM and LM Studio all expose the **OpenAI-compatible** `/v1` API, so a
single OpenAI-wire adapter configured by base URL + key covers all of them. Backend-specific
adapters would be redundant code with no behavioural gain at this scope.

## Consequences

### Positive
- The gateway can front a real model today (Ollama, no key) and any OpenAI-compatible backend by
  config alone (OpenAI/vLLM/LM Studio) — no code change to switch backends.
- ACL preserved: resilience, metering, and the edge stay provider-agnostic; only this adapter
  knows the OpenAI wire details and auth.
- `stream_options.include_usage` gives real token accounting for streamed completions, closing the
  metering gap for the streaming path where the backend supports it.
- The updated mock upstream exercises the *real* OpenAI mapping in unit + load tests, so tests are
  representative of production wiring while `make up` stays fast.

### Negative
- The adapter must tolerate backend variance (some omit usage on stream, some shape `stop` or
  errors slightly differently); the graceful-0 usage path and error mapping add code and tests.
- Two mocks now coexist (HTTP mock upstream + interface-level `Mock`); contributors must pick the
  right one. Documented in the provider package.
- Real-backend behaviour (latency, model availability) is environment-dependent and not asserted in
  CI; the Ollama profile is an optional, documented demo path, not a CI gate.

### Neutral
- Multi-provider routing by model prefix and automatic failover remain future non-goals (already
  tracked as follow-ups).
- The injected `*http.Client` and its timeout/pooling (ADR-0010, NFR-004) are reused as-is.
- `GATEWAY_UPSTREAM_URL` already existed; `GATEWAY_UPSTREAM_API_KEY` and `GATEWAY_UPSTREAM_MODEL`
  are added.

## References

- DEC-013 (resolved by this ADR)
- ADR-0009 (single Provider interface — this is a new implementation behind that port)
- ADR-0010 (injected client / repository-per-context — reused for the upstream client)
- ADR-0012 (OpenAI-compatible edge contract — this is its upstream counterpart)
- FR-021 (real OpenAI-compatible upstream provider adapter)
- FR-019 (streaming usage via stream_options.include_usage)
- CON-001/CON-002 (stdlib-first, minimal deps — adapter uses net/http + encoding/json)
- EXT-001 (LlmProvider — now a real OpenAI-compatible backend)
- CTX-001 (Proxy — owns the Provider port)

## History

- 2026-06-26: Created — real OpenAI-wire HTTP provider adapter configured by base URL + optional
  bearer key + model; Ollama primary showcase (no key); OpenAI/vLLM/LM Studio via config;
  stream_options.include_usage for streaming metering (graceful 0 if absent); mock upstream updated
  to the real OpenAI shape; interface-level Mock retained.
