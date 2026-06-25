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

## HTTPProvider (CARD-013)

`HTTPProvider` is the production wiring of the ACL: a `Provider` that reaches a mock LLM
upstream over **real HTTP** through an **injected** `*http.Client`, so the tuned client's
connection pooling (ADR-0010, NFR-004) is genuinely exercised rather than discarded.

- `NewHTTP(client *http.Client, baseURL string)` — the client is injected (no package
  transport); pooling + the explicit total timeout are owned by `cmd/gateway`.
- `Infer` POSTs canonical-mapped JSON to `/v1/chat/completions`, maps the upstream JSON
  response → canonical `Response` (incl. `Usage`).
- `InferStream` requests the SSE variant and reads the `text/event-stream` body
  incrementally, emitting one canonical `Chunk` per `data:` event and a terminal `Done`
  chunk with `Usage`; the reader goroutine selects on `ctx.Done()` and closes the body +
  channel on exit (no leak).
- Every call builds its request with `http.NewRequestWithContext`, so a cancelled ctx
  aborts the in-flight upstream call and surfaces a ctx-wrapping error (FR-003).
- Non-2xx upstream statuses become `*StatusError` (5xx retryable, 4xx not), keeping the
  resilience layer provider-agnostic.

The upstream wire types (`wireRequest`/`wireResponse`/`wireStreamEvent`) are deliberately
distinct from the canonical types and never cross the `Provider` boundary (ADR-0009).

## Mock upstream (CARD-013)

`MockUpstreamHandler(MockUpstreamOptions)` returns an `http.Handler` implementing the mock
upstream wire API consumed by `HTTPProvider`. It is the reproducible mock served over real
HTTP — usable in tests (`httptest.NewServer`) and run in-process by `cmd/gateway` on a
loopback side-listener when `GATEWAY_UPSTREAM_URL` is unset. Controllable `Latency`,
`FailStatus` (error-rate), and `StreamChunks`; serves both the JSON and SSE paths.

## See also

- `internal/proxy/retry` — uses `StatusError.Retryable()` for retry classification
- ADR-0009: `meta/architecture/decisions/adr/0009-single-provider-interface.md`
