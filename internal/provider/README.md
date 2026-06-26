# internal/provider

The **anti-corruption layer** (ADR-0009) between the gateway core and external LLM
providers (EXT-001). Everything inside the gateway speaks the canonical types defined
here; provider-specific wire formats (OpenAI / Anthropic JSON, SSE) are normalized into
these shapes by adapter code, so no provider-specific type ever crosses this boundary.

## Port

```go
type Provider interface {
    Infer(ctx context.Context, req Request) (Response, error)          // unary
    InferStream(ctx context.Context, req Request) (<-chan Chunk, error) // streaming (SSE)
}
```

Both methods honor `ctx` (FR-003): `Infer` returns `ctx.Err()` if cancelled mid-call;
`InferStream` stops emitting and **always closes** the returned channel on completion,
error, or cancellation.

## Canonical types

- `Request` — `Model`, `Messages []Message`, `Stream`, `MaxTokens`, `*Temperature` (provider-agnostic).
- `Response` — `Model`, `Content`, `FinishReason`, normalized `Usage` (so metering/CARD-010 reads canonical usage only).
- `Chunk` — one streamed delta: `Content`, `Done` (terminal), `Usage` (on terminal chunk), `Err` (mid-stream failure).
- `Message` / `Role` (system|user|assistant) / `Usage{Prompt,Completion,TotalTokens}`.
- `StatusError{Code, Message}` — canonical upstream HTTP error. `Retryable()` returns `true` for 5xx, `false` for 4xx (AC-021). Used by the retry engine to classify failures without string-matching.

## Mock

`Mock` is the configurable test double (used by the CARD-003/004 proxy tests):
controllable per-call `Latency` and `ErrorRate` (deterministic gate, no RNG), `Err`
sentinel, and `StreamChunks`. Constructed via `New` + functional options
(`WithResponse`, `WithLatency`, `WithError`, `WithStreamChunks`). The streaming producer
selects on `ctx.Done()` at every send and closes the channel via `defer` (no goroutine leak).

> **Consumer contract:** a streaming caller that abandons the channel early MUST cancel
> the context, otherwise the producer parks on an unbuffered send (see provider.go).

Real adapters (OpenAI, Anthropic) are deferred; they will implement `Provider` and
translate wire-format ↔ canonical types here.

## HTTPProvider (CARD-013, CARD-016)

`HTTPProvider` is the production wiring of the ACL: a `Provider` that reaches a **real
OpenAI-compatible** upstream over **real HTTP** through an **injected** `*http.Client`, so
the tuned client's connection pooling (ADR-0010, NFR-004) is genuinely exercised rather than
discarded. It speaks the real OpenAI `/v1/chat/completions` wire (ADR-0012/ADR-0013), so the
configured backend can be **Ollama** (default showcase, no key), OpenAI, vLLM, or LM Studio.

- `NewHTTP(client *http.Client, baseURL string, opts…)` — the client is injected (no package
  transport); pooling + the explicit total timeout are owned by `cmd/gateway`. `baseURL`
  includes the `/v1` segment; `WithAPIKey` and `WithModel` configure optional bearer auth and
  the model. The request is sent to `<baseURL>/chat/completions`.
- `Infer` POSTs the canonical request as an OpenAI request, then maps the OpenAI response
  (`choices[0].message.content` + `finish_reason` + `usage`) → canonical `Response`.
- `InferStream` requests the SSE variant with `stream_options.include_usage=true` and reads
  the `text/event-stream` body incrementally, emitting one canonical `Chunk` per
  `chat.completion.chunk` `delta.content` and a terminal `Done` chunk with `Usage` parsed from
  the trailing usage chunk; it stops on a literal `data: [DONE]`. If the backend omits the
  usage chunk, the stream ends gracefully with zero usage. The reader goroutine selects on
  `ctx.Done()` and closes the body + channel on exit (no leak).
- Every call builds its request with `http.NewRequestWithContext`, so a cancelled ctx aborts
  the in-flight upstream call and surfaces a ctx-wrapping error (FR-003).
- Non-2xx upstream statuses become `*StatusError` (5xx retryable, 4xx not), parsing the OpenAI
  `{error:{message,type,code}}` envelope into the message — keeping the resilience layer
  provider-agnostic.
- `Authorization: Bearer <key>` is sent only when a non-empty key is configured (omitted for
  Ollama).

The upstream wire types (`oaiRequest`/`oaiResponse`/`oaiStreamEvent`/`oaiErrorEnvelope`) are
deliberately private, distinct from the canonical types, and never cross the `Provider`
boundary (ADR-0009).

## Mock upstream (CARD-013, CARD-016)

`MockUpstreamHandler(MockUpstreamOptions)` returns an `http.Handler` that emits the **real
OpenAI wire shape** (unary `chat.completion` + SSE `chat.completion.chunk` deltas + a usage
chunk + `data: [DONE]`) consumed by `HTTPProvider`. It is the reproducible mock served over
real HTTP — usable in tests (`httptest.NewServer`) and run in-process by `cmd/gateway` on a
loopback side-listener when `GATEWAY_UPSTREAM_URL` is unset (the default `make up` upstream, so
the demo stays fast). Controllable `Latency`, `FailStatus` (error-rate), `StreamChunks`, and
`OmitStreamUsage` (models a backend ignoring `include_usage`); serves both the JSON and SSE paths.

## See also

- `internal/proxy/retry` — uses `StatusError.Retryable()` for retry classification
- ADR-0009: `meta/architecture/decisions/adr/0009-single-provider-interface.md`
