# CAP-001 — Inference proxying

> Source of truth: `cmd/gateway/main.go`, `internal/middleware/cache.go`,
> `internal/server/server.go`, `internal/server/edge.go`,
> `internal/proxy/router.go`, `internal/proxy/resilience/resilience.go`,
> `internal/proxy/retry/retry.go`, `internal/provider/provider.go`,
> `internal/provider/httpprovider.go`, `internal/provider/mockupstream.go`,
> `api/openapi.yaml`, `internal/api/api.gen.go`.

## 1. Why this exists

sluice is a **drop-in OpenAI-compatible** LLM gateway: it accepts the *real*
OpenAI `POST /v1/chat/completions` wire request (ADR-0012), turns it into a
routed, resilient, observable, optionally-cached upstream call, and speaks the
*real* OpenAI wire to an OpenAI-compatible backend (Ollama / OpenAI / vLLM / LM
Studio — ADR-0013). CAP-001 is that hot path. The design goal (NFR-001) is to add
**≤ 20 ms p95 overhead** excluding provider latency, so every layer on the path is
either a cheap header/body check or a non-blocking hand-off.

Two architectural rules shape the path:

- **ADR-0012 (OpenAI-compatible contract, v1 subset).** The OpenAI wire shape —
  `chat.completion` objects, `chat.completion.chunk` SSE events, the
  `{error:{message,type,code}}` envelope, and the edge-generated
  `id`/`created`/`object` — lives **only** at the edge (`internal/server/edge.go`).
  The request is liberal-accept: unknown fields are ignored, never a 400.
- **ADR-0009 (one `Provider` interface as the anti-corruption layer).** Only the
  canonical `provider.Request`/`Response`/`Chunk` cross the Provider boundary, so
  the proxy core, resilience, metering, and observability stay provider-agnostic.

The HTTP boundary itself is **generated** from `api/openapi.yaml` (ADR-0011), so
the Go code in this context owns only the behaviour *behind* that contract — and
the edge mapping between the generated OpenAI DTOs and the canonical types.

A request crosses, in order: the **middleware chain** → the **cache** → the
**handler** (`Server.CreateChatCompletion`) → the **OpenAI edge mapping**
(`edge.go`) → the **router** (model → provider) → the **resilience seam**
(`InferFunc`/`StreamFunc`) → the **OpenAI-compatible upstream adapter**
(`httpprovider.go`) → the upstream. See
[diagrams/01-inference-proxying-01.puml](diagrams/01-inference-proxying-01.puml)
for the non-streaming sequence and
[diagrams/01-inference-proxying-02.puml](diagrams/01-inference-proxying-02.puml)
for the streaming sub-path.

## 2. The middleware chain (composition order)

The chain is assembled in the composition root, `run()` in `cmd/gateway/main.go`,
outermost first:

```go
// cmd/gateway/main.go
handler := middleware.Recoverer(logger)(
    logging.Middleware(logger)(
        middleware.Tracing(tracer.Tracer())(
            met.Middleware(
                rateLimiter.Middleware(
                    manager.CountingMiddleware(
                        cacheMW.Middleware(appHandler),
                    ),
                ),
            ),
        ),
    ),
)
```

The order is the one mandated by ADR-0006:

> `recover → logging → tracing → metrics → rate-limit → counting → cache → routes`

Why this order matters for CAP-001:

- **Panic recovery is outermost** so any handler panic becomes a 500 — now an
  OpenAI error envelope (CAP-005, file `02-runtime-lifecycle.md`) — and the
  process survives.
- **Rate-limit runs before any provider/pool work** (INV-004): a 429 never reaches
  the proxy core. Rate limiting lives in the
  [Resilience](../../02-resilience/resilience/) context; ADR-0006 puts it in the
  chain because the API key is in the `Authorization` header, available *before*
  the body is parsed.
- **Counting middleware** (`manager.CountingMiddleware`) increments the in-flight
  gauge that graceful shutdown drains (CAP-005).
- **Cache is innermost**, just before the generated routes, so a cache HIT
  short-circuits the provider path while still being wrapped by the outer
  logging/metrics/tracing instrumentation.

Note that the **OpenAPI request validator** wraps the generated routes (built in
`Server.Handler`): a body that violates the schema (unknown enum role, array
`content`, wrong type) is rejected **400** *before* `CreateChatCompletion` runs —
and that 400 is rendered as an OpenAI error envelope (Section 8).

## 3. Cache lookup (FR-005, ADR-0004 / ADR-0010)

`CacheMiddleware.Middleware` (`internal/middleware/cache.go`) acts on exactly one
route — `POST /v1/chat/completions`. Everything else passes straight through:

```go
if m.repo == nil || r.Method != cacheMethod || r.URL.Path != cacheRoute {
    next.ServeHTTP(w, r)
    return
}
```

**Key.** The cache key is a hex sha256 of the request identity — method, path, and
the raw body bytes (NUL-separated):

```go
func cacheKey(method, path string, body []byte) string {
    h := sha256.New()
    h.Write([]byte(method)); h.Write([]byte{0})
    h.Write([]byte(path));   h.Write([]byte{0})
    h.Write(body)
    return hex.EncodeToString(h.Sum(nil))
}
```

The body is read with a `LimitReader` capped at `maxBodyBytes` (default 1 MiB) and
then **restored** (`r.Body = io.NopCloser(bytes.NewReader(body))`) so the
downstream handler can re-read it. Oversize bodies are reconstructed in full and
bypass the cache — never a 413 from this layer. The key intentionally does **not**
include the API key: identical request bodies produce identical completions, so
entries are shared cross-tenant by design (documented trade-off in the source).
Because the key hashes the **raw** body bytes, two semantically-identical OpenAI
requests that differ only in JSON whitespace/key-order are distinct entries — a
documented hit-rate (not correctness) limitation.

**Streaming bypass.** A tolerant `streamProbe` parses only the `stream` flag; a
streaming request never computes a key or stores a response (AC-016):

```go
var probe streamProbe
if jsonErr := json.Unmarshal(body, &probe); jsonErr == nil && probe.Stream {
    next.ServeHTTP(w, r)
    return
}
```

**HIT / MISS.** On a HIT the stored `cacheEnvelope` (Content-Type + base64 body)
is decoded and replayed with `X-Cache: HIT` — so a cached OpenAI `chat.completion`
JSON is returned verbatim, headers identical to a MISS. On a MISS the response is
set to `X-Cache: MISS` and captured by a `cacheRecorder` that streams bytes to the
client *and* buffers them; a successful 200 is then stored:

```go
ttl := m.resolveTTL(r)
ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
defer cancel()
if setErr := m.repo.Set(ctx, key, envelope, ttl); setErr != nil { /* log, serve anyway */ }
```

The store is detached from the request context (`context.WithoutCancel`) so the
already-completed request's cancellation cannot abort it, but bounded to 2 s so a
slow Redis cannot leak goroutines. **TTL** comes from `resolveTTL`: the
`X-Cache-TTL` request header (whole seconds) when present and positive, otherwise
the configured default (5 minutes — ADR-0004):

```go
func (m *CacheMiddleware) resolveTTL(r *http.Request) time.Duration {
    if v := r.Header.Get(ttlOverrideHeader); v != "" {
        if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
            return time.Duration(secs) * time.Second
        }
    }
    return m.defaultTTL
}
```

**Resilience.** The middleware depends only on the `cache.CacheRepository` port
(ADR-0010), never on go-redis. Any error on `Get` or `Set` is logged and the
request falls through to the live handler, so a Redis outage is never a client
error (AC-017). A corrupt/old-format envelope is treated as a MISS.

> Caching is unchanged by the OpenAI increment: it keys and stores opaque bytes
> and is oblivious to the OpenAI wire shape. The cached body just happens to now be
> an OpenAI `chat.completion` object.

## 4. The OpenAI edge mapping — request side (ADR-0012, `edge.go`)

The generated strict server has already decoded the JSON body into
`api.ChatCompletionRequest` (the real OpenAI request DTO). The handler first does
the cheap presence checks, then crosses the edge into the canonical request with
`toCanonicalRequest` (`internal/server/edge.go`):

```go
// internal/server/server.go — CreateChatCompletion
if body.Model == "" {
    return badRequest("the 'model' field is required", "invalid_request_error", "missing_model"), nil
}
if len(body.Messages) == 0 {
    return badRequest("the 'messages' field must contain at least one message", "invalid_request_error", "empty_messages"), nil
}
req, eerr := toCanonicalRequest(body)   // ADR-0009 ACL, liberal-accept
if eerr != nil {
    return badRequest(eerr.message, eerr.typ, eerr.code), nil   // n>1 → 400, no provider contacted
}
```

`toCanonicalRequest` forwards **only the modeled v1-subset fields** (ADR-0012 §2)
onto `provider.Request`: `model`, `messages`, `stream`, `temperature`, `top_p`,
`max_tokens`, `stop`. The generated DTO uses pointers for optionals, so the mapper
de-references them and widens `float32` → `*float64`:

```go
func toCanonicalRequest(body api.ChatCompletionRequest) (provider.Request, *edgeError) {
    if eerr := rejectUnsupported(body); eerr != nil {
        return provider.Request{}, eerr
    }
    req := provider.Request{Model: body.Model, Messages: ...}
    if body.Stream != nil      { req.Stream = *body.Stream }
    if body.MaxTokens != nil   { req.MaxTokens = *body.MaxTokens }
    if body.Temperature != nil { t := float64(*body.Temperature); req.Temperature = &t }
    if body.TopP != nil        { p := float64(*body.TopP);        req.TopP = &p }
    if body.Stop != nil        { req.Stop = normalizeStop(body.Stop) }
    ...
}
```

**Liberal-accept (§3).** Unknown OpenAI fields (`seed`, `user`, penalties,
`response_format`, `logprobs`, …) arrive in `body.AdditionalProperties`
(the generated DTO sets `additionalProperties: true`) and are simply **not**
forwarded — never a 400. This is exactly what lets unmodified OpenAI SDKs talk to
sluice.

**`n > 1` → OpenAI-shaped 400 (§4, AC-055).** The one documented non-goal that
*can* reach the handler (multimodal/array `content` is already rejected by the
request validator) is `n > 1`, detected in `rejectUnsupported` off the
additional-properties map, **without contacting any provider**:

```go
func rejectUnsupported(body api.ChatCompletionRequest) *edgeError {
    if v, ok := body.AdditionalProperties["n"]; ok {
        if n, isNum := v.(float64); isNum && n > 1 {
            return &edgeError{
                httpStatus: 400,
                message:    "n>1 is not supported: the gateway returns a single choice",
                typ:        "invalid_request_error",
                code:       "unsupported_value",
            }
        }
    }
    return nil
}
```

**Scalar-or-array `stop` (the oneOf union).** OpenAI's `stop` is
`string | []string`. The generated DTO models it as a union
(`ChatCompletionRequest_Stop`); `normalizeStop` collapses it to the canonical
`[]string`:

```go
func normalizeStop(s *api.ChatCompletionRequest_Stop) []string {
    if s == nil { return nil }
    if arr, err := s.AsChatCompletionRequestStop1(); err == nil && len(arr) > 0 {
        return append([]string(nil), arr...)     // array form
    }
    if str, err := s.AsChatCompletionRequestStop0(); err == nil && str != "" {
        return []string{str}                      // scalar form wrapped
    }
    return nil                                     // empty/absent → unset
}
```

## 5. Routing by model (FR-002)

After the canonical request is built, the handler resolves the provider:

```go
prov, err := s.router.Provider(body.Model)
if err != nil {
    if errors.Is(err, proxy.ErrModelNotRegistered) {
        return notFound("no provider is registered for model " + body.Model), nil  // 404
    }
    return badGateway("failed to route request"), nil
}
```

The `Router` (`internal/proxy/router.go`) is a small concurrency-safe registry —
`map[string]provider.Provider` behind an `RWMutex`. `Register` happens at startup;
in v1 a single OpenAI-compatible upstream is registered for the configured model
(`cmd/gateway/main.go`):

```go
upstreamModel := cfg.Upstream.Model   // "mock" by default, "llama3.2" for an external backend
router.Register(upstreamModel, provider.NewHTTP(httpClient, upstreamURL,
    provider.WithAPIKey(cfg.Upstream.APIKey),   // upstream bearer key — omitted when empty (Ollama)
    provider.WithModel(upstreamModel),
))
```

`Provider` runs on every request and returns `ErrModelNotRegistered` for an
unknown model, which the handler maps to **404**.

## 6. Crossing the anti-corruption layer (ADR-0009)

The canonical `provider.Request` now carries the fields the OpenAI edge forwards —
including the new `TopP` and `Stop` (provider.go):

```go
type Request struct {
    Model       string
    Messages    []Message
    Stream      bool
    MaxTokens   int
    Temperature *float64
    TopP        *float64   // forwarded to the OpenAI upstream `top_p`
    Stop        []string   // forwarded to the OpenAI upstream `stop`
}
```

The `Provider` interface is deliberately two methods, one per path:

```go
type Provider interface {
    Infer(ctx context.Context, req Request) (Response, error)
    InferStream(ctx context.Context, req Request) (<-chan Chunk, error)
}
```

The canonical `Usage{PromptTokens, CompletionTokens, TotalTokens}` is what
metering reads (Section 10), so the metering context never imports a provider
package. Upstream non-2xx statuses become a `*StatusError` whose `Retryable()`
returns true only for 5xx — this is how the retry/breaker layer classifies
failures without string-matching.

## 7. The resilience seam (`InferFunc` / `StreamFunc`, ADR-0006)

`Server` calls the provider through two function-typed seams rather than calling
`provider.Provider` directly:

```go
// internal/server/server.go
type InferFunc  func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error)
type StreamFunc func(ctx context.Context, p provider.Provider, req provider.Request) (<-chan provider.Chunk, error)
```

By default `New` sets these to call `p.Infer` / `p.InferStream` directly, but
`WithInferFunc` / `WithStreamFunc` let the composition root wrap them with
resilience and instrumentation **without touching the handler**. In `main.go`:

```go
guardedInfer  := workerPool.Guard(composer.InferFunc())          // pool → retry → breaker → provider
instrumentedInfer := tracer.InstrumentInferFunc(met.InstrumentInferFunc(guardedInfer))

guardedStream := workerPool.GuardStream(composer.StreamFunc())   // pool → breaker → provider (NO retry)
instrumentedStream := tracer.InstrumentStreamFunc(met.InstrumentStreamFunc(guardedStream))

srv := server.New(router, healthHandler, logger,
    server.WithInferFunc(instrumentedInfer),
    server.WithStreamFunc(instrumentedStream),
    ...)
```

The `resilience.Composer` (`internal/proxy/resilience/resilience.go`) builds the
unary seam as `retry(breaker.Execute(providerCall))`, keyed by the request model,
and the streaming seam as `breaker.ExecuteStream(provider.InferStream)` with **no
retry**. The breaker, retry engine, and worker pool themselves live in the
[Resilience](../../02-resilience/resilience/) context; this aspect documents only
how the proxy *composes and consumes* them.

## 8. Error mapping → OpenAI error envelope (503 / 502 / 400 / 404)

The seam classifies the failure; the handler maps it to an HTTP status **and** an
OpenAI `{error:{message,type,code}}` envelope (ADR-0012 §7, FR-020). The envelope
is built by `openAIError` in `edge.go`; the handler's `badRequest`/`notFound`/
`badGateway`/`serviceUnavailable` helpers wrap it into the generated responses:

| Cause | Seam result | Handler → HTTP | OpenAI `type` / `code` |
|-------|-------------|----------------|------------------------|
| Schema-invalid body | (OpenAPI validator) | **400** | `invalid_request_error` / `validation_error` |
| Missing/empty model or messages | (handler) | **400** | `invalid_request_error` / `missing_model` … |
| `n > 1` | `edgeError` (no provider call) | **400** | `invalid_request_error` / `unsupported_value` |
| Unknown model | `proxy.ErrModelNotRegistered` | **404** | `invalid_request_error` / `model_not_found` |
| Open breaker | `*Unavailable{breaker_open}` | **503** + `Retry-After` | `service_unavailable` / `service_unavailable` |
| Client deadline/cancel during retry | `*Unavailable{deadline}` | **503** + `Retry-After` | `service_unavailable` / `service_unavailable` |
| Worker pool saturated | `pool.ErrPoolSaturated` (matches `ErrServiceUnavailable`) | **503** + `Retry-After` | `service_unavailable` / `service_unavailable` |
| 5xx upstream, retries exhausted | wrapped `retry.ErrExhausted` | **502** | `upstream_error` / `bad_gateway` |
| 4xx `StatusError` | returned unwrapped (not retried) | **502** | `upstream_error` / `bad_gateway` |
| Rate limit exceeded | (rate-limit middleware, before the proxy) | **429** + `Retry-After` | (rate-limit envelope) |

The 503 check is intentionally **before** the generic 502 so resilience signals
are not masked as provider errors:

```go
resp, err := s.infer(ctx, prov, req)
if err != nil {
    if errors.Is(err, ErrServiceUnavailable) {     // open breaker / deadline / pool saturation
        return s.serviceUnavailable(err), nil       // 503 + Retry-After
    }
    return badGateway("upstream provider request failed"), nil  // 502
}
```

`ErrServiceUnavailable` is a sentinel defined in the `server` package (not the
resilience package) to avoid an import cycle: `resilience.Unavailable` implements
`Is(target) bool` returning `target == server.ErrServiceUnavailable` and
`RetryAfter() time.Duration`. `serviceUnavailableResponse.VisitCreateChatCompletionResponse`
sets the `Retry-After` header (rounded to whole seconds, floored at 1) and then
emits the generated 503 JSON body.

The schema-validation 400 is produced by the request validator's `OnErr` hook in
`Server.Handler`, which renders a **concise** OpenAI envelope (the raw kin-openapi
schema dump is deliberately not leaked):

```go
openapi3filter.OnErr(func(_ context.Context, w http.ResponseWriter, status int, _ openapi3filter.ErrCode, err error) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(openAIError(
        "the request body did not conform to the schema", "invalid_request_error", "validation_error"))
})
```

## 9. Unary response — OpenAI `chat.completion` (ADR-0012 §5)

On success the handler maps the canonical `provider.Response` back to a real OpenAI
`chat.completion` object with `toUnaryResponse` (`edge.go`). Critically, the
`id`, `created`, and `object` are **generated at the edge** — never passed through
from the upstream — so the contract is stable regardless of which backend
(Ollama/OpenAI/vLLM) produced the completion:

```go
func toUnaryResponse(resp provider.Response) api.ChatCompletionResponse {
    finish := resp.FinishReason
    return api.ChatCompletionResponse{
        Id:      newID(),                      // "chatcmpl-" + crypto/rand hex
        Object:  objectChatCompletion,         // "chat.completion"
        Created: time.Now().Unix(),
        Model:   resp.Model,
        Choices: []api.Choice{{
            Index:        0,                    // exactly one choice
            Message:      api.ResponseMessage{Role: roleAssistant, Content: resp.Content},
            FinishReason: &finish,
        }},
        Usage: api.Usage{
            PromptTokens:     resp.Usage.PromptTokens,
            CompletionTokens: resp.Usage.CompletionTokens,
            TotalTokens:      resp.Usage.TotalTokens,
        },
    }
}
```

`newID` uses `crypto/rand` so concurrent requests never collide, falling back to a
timestamp if the RNG ever fails (the id is informational, not a security token):

```go
func newID() string {
    var b [12]byte
    if _, err := rand.Read(b[:]); err != nil {
        return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
    }
    return "chatcmpl-" + hex.EncodeToString(b[:])
}
```

The handler returns it as the generated 200 response:

```go
s.recordUsage(ctx, body.Model, resp.Model, resp.Usage, time.Since(start), http.StatusOK)
return api.CreateChatCompletion200JSONResponse(toUnaryResponse(resp)), nil
```

## 10. Async usage metering on the success path (FR-014)

`recordUsage` builds a `metering.UsageEvent` (provider alias = the model routing
key, resolved model, canonical token counts, latency, status, request-id from
context) and calls `s.meter.Enqueue`. The sink (`metering.Sink`) is **non-blocking
by contract** — on a full buffer the event is dropped (INV-003 / CON-006) — so
metering never delays the hot path. It defaults to `metering.NopSink{}` and is
injected via `WithMeteringSink(meteringBuffer)` in `main.go`. The downstream
worker, batching, and Postgres persistence are documented in
[Metering](../../04-integrations/metering/).

## 11. The OpenAI-compatible upstream adapter (ADR-0013, `httpprovider.go`)

`HTTPProvider` is the production wiring of the ADR-0009 ACL. It speaks the **real**
OpenAI `/v1/chat/completions` wire to an OpenAI-compatible backend over an
**injected** `*http.Client` (so process-wide pooling — ADR-0010/NFR-004 — is
genuinely exercised). It POSTs to `<baseURL>/chat/completions` where `baseURL`
already includes the `/v1` segment.

**Request mapping (`toOAIRequest`).** Only the modeled canonical fields become the
private OpenAI request type `oaiRequest`. The OpenAI shape never leaves this file:

```go
type oaiRequest struct {
    Model         string            `json:"model"`
    Messages      []oaiMessage      `json:"messages"`
    Stream        bool              `json:"stream,omitempty"`
    StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
    Temperature   *float64          `json:"temperature,omitempty"`
    TopP          *float64          `json:"top_p,omitempty"`
    MaxTokens     int               `json:"max_tokens,omitempty"`
    Stop          []string          `json:"stop,omitempty"`
}
```

When the request carries no `Model`, the adapter falls back to its configured
`WithModel` value; `Request.Model` always takes precedence (passed through as-is).

**Bearer-optional auth (ADR-0013 §3).** `newRequest` attaches
`Authorization: Bearer <key>` **only when a key is configured** — Ollama needs
none, OpenAI/vLLM are configured with one:

```go
func (p *HTTPProvider) newRequest(ctx context.Context, body []byte, accept string) (*http.Request, error) {
    httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
        p.baseURL+"/chat/completions", bytes.NewReader(body))
    ...
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("Accept", accept)
    if p.apiKey != "" {
        httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
    }
    return httpReq, nil
}
```

**Unary mapping (`Infer`).** It marshals `toOAIRequest(req, false)`, POSTs it,
maps a non-2xx to a `*StatusError` (parsing the OpenAI `{error:{message,…}}` body
for a useful message via `statusErrorFromResponse`), and decodes the OpenAI
`oaiResponse` into a canonical `Response` — `choices[0].message.content` +
`finish_reason` + normalised `usage`:

```go
return Response{
    Model:        or.Model,
    Content:      content,                 // or.Choices[0].Message.Content
    FinishReason: finish,                  // or.Choices[0].FinishReason
    Usage:        toCanonicalUsage(&or.Usage),
}, nil
```

**The default upstream is the mock** (`mockupstream.go`), now emitting the *real*
OpenAI shape (unary `chat.completion`, SSE `chat.completion.chunk` events,
`[DONE]`, and a trailing usage chunk) so unit + load tests exercise the real
adapter mapping. `make up` stays fast and dependency-free; an external backend is
selected by setting `GATEWAY_UPSTREAM_URL`.

## 12. Non-streaming vs streaming response

**Non-streaming** is the path above: a buffered canonical `Response` mapped to a
`CreateChatCompletion200JSONResponse` carrying an OpenAI `chat.completion`.

**Streaming** cannot return a buffered object — it must take over the raw
`http.ResponseWriter`. The handler branches on `req.Stream` and returns a custom
`streamResponse` that implements the generated response-visitor seam:

```go
if req.Stream {
    return streamResponse{
        ctx: ctx, stream: s.stream, prov: prov, req: req,
        logger: s.logger, model: body.Model, meter: s.meter, start: time.Now(),
    }, nil
}
```

`streamResponse.VisitCreateChatCompletionResponse` drives the SSE loop. The
critical ordering (AC-014a/b) is that it **initiates the stream first, before
writing any byte**, so an initiation failure is still a real HTTP status, not a
half-open 200:

```go
ch, err := r.stream(r.ctx, r.prov, r.req)
if err != nil {
    return r.writeInitError(w, err)   // 503 (open breaker / pool) or 502 (provider) — OpenAI envelope, no SSE bytes
}
w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-cache")
w.Header().Set("Connection", "keep-alive")
w.WriteHeader(http.StatusOK)
rc := http.NewResponseController(w)
_ = rc.Flush()   // flush headers so the client sees the stream open
```

`writeInitError` writes `Content-Type: application/json` (not `text/event-stream`)
and an OpenAI error envelope, so the client gets a clean 503/502, never a dead
half-open stream.

**Stable id/created per stream (ADR-0012 §6).** A single `streamShaper` is seeded
once per stream so every emitted `chat.completion.chunk` shares the same
edge-generated `id` and `created`, letting the client correlate deltas:

```go
shaper := newStreamShaper(r.model)   // id = newID(); created = time.Now().Unix()
```

**Per-chunk OpenAI chunk + flush.** The forwarding loop selects on `ctx.Done()`
versus the chunk channel; each canonical `Chunk` becomes an OpenAI
`chat.completion.chunk` SSE event, flushed after every write:

```go
case chunk, ok := <-ch:
    if !ok { _, _ = w.Write([]byte(sseDone)); _ = rc.Flush(); return nil }     // normal end → [DONE]
    if chunk.Err != nil { /* log; emit [DONE]; return */ }
    if chunk.Done {
        payload, _ := json.Marshal(shaper.finalChunk())   // empty delta + finish_reason "stop"
        fmt.Fprintf(w, "data: %s\n\n", payload); rc.Flush()
        _, _ = w.Write([]byte(sseDone)); rc.Flush()        // literal data: [DONE]
        r.recordUsage(chunk.Usage)                          // non-blocking metering
        return nil
    }
    payload, _ := json.Marshal(shaper.contentChunk(chunk.Content))   // {choices:[{delta:{content}}]}
    if _, wErr := fmt.Fprintf(w, "data: %s\n\n", payload); wErr != nil { return nil }
    _ = rc.Flush()
```

The chunk wire shapes live in `edge.go` (`streamChunk`/`chunkChoice`/`chunkDelta`,
`object: "chat.completion.chunk"`), so the OpenAI streaming envelope is owned by
the edge, not the provider. Two details are load-bearing:

- Flushing goes through **`http.NewResponseController(w).Flush()`**, not a
  `w.(http.Flusher)` assertion, because the metrics/tracing/cache middleware wrap
  the writer and implement `Unwrap() http.ResponseWriter`; the controller
  traverses that unwrap chain to reach the real flusher (`cacheRecorder.Unwrap`
  is exactly that hook).
- The stream terminates with the conventional marker
  `const sseDone = "data: [DONE]\n\n"` on every exit path.

See [diagrams/01-inference-proxying-02.puml](diagrams/01-inference-proxying-02.puml).

## 13. Streaming usage for metering — `stream_options.include_usage` (FR-019)

On the streaming path the adapter sets `stream_options.include_usage = true`
(`toOAIRequest` when `stream` is set) so the backend emits a **trailing usage
chunk** (empty `choices`, populated `usage`). `streamLoop` captures it and emits it
as the terminal `Chunk.Usage`:

```go
if ev.Usage != nil {
    usage = toCanonicalUsage(ev.Usage)   // captured for the terminal Done chunk
}
if len(ev.Choices) == 0 {
    continue                              // usage-only chunk carries no delta
}
...
case "[DONE]":
    sendChunk(ctx, out, Chunk{Done: true, Usage: usage})   // terminal Done with usage
```

If the backend ignores `include_usage` (some Ollama builds — modelled by the mock
upstream's `OmitStreamUsage`), the stream still ends normally with a `Done` chunk
carrying zero/uncounted usage — no error, no stream break. Metering records it
gracefully. The streaming-side `recordUsage` in `streamResponse` enqueues this
terminal usage as a best-effort, non-blocking `UsageEvent`.

## 14. Retry with backoff on the unary path (FR-006)

The Retry Engine (`internal/proxy/retry/retry.go`) wraps a generic
`Call func(ctx) (provider.Response, error)` — the same shape the breaker wraps,
which is what lets them compose. `Do` is bounded, deadline-aware, and classifies
errors by type, not string. `shouldRetry` returns false for the composition root's
non-retryable predicate (an **open breaker** — `resilience.IsOpenState`, wired via
`retry.WithNonRetryable`), for context cancellation/deadline, and for a 4xx
`StatusError`; a 5xx `StatusError` is retryable. Exhausting the budget wraps the
last error in `ErrExhausted`, which the handler maps to 502. The "don't retry into
an open breaker" rule (ADR-0006) is enforced because `breaker.ErrOpenState` is
marked non-retryable, so the retry layer propagates it unwrapped instead of
spinning. **The streaming path has no retry** — a partially-sent OpenAI SSE stream
cannot be safely replayed.

## 15. Client-cancellation aborts the real upstream (FR-003, INV-002)

Cancellation is end-to-end via `context.Context`. The per-request `ctx` is threaded
into the `InferFunc`/`StreamFunc` and on into the provider. The `HTTPProvider`
builds every upstream request with `http.NewRequestWithContext` (Section 11), so a
cancelled ctx aborts the in-flight upstream HTTP call to Ollama/OpenAI/vLLM:

```go
resp, err := p.client.Do(httpReq)
if err != nil { return Response{}, mapTransportError(err) }
```

`mapTransportError` surfaces `context.Canceled` / `context.DeadlineExceeded`
unwrapped so callers (and the retry classifier) can match them with `errors.Is`.
On the streaming side, `streamLoop` checks `ctx.Err()` between SSE events and
`sendChunk` selects on `ctx.Done()`, then `defer drainAndClose(body)` /
`defer close(out)` ensure the upstream body is closed and the channel closed on
exit — no goroutine leak. When the client disconnects, the handler's
`case <-r.ctx.Done()` returns, which (because the same ctx was passed to the
stream) cancels the upstream and lets the provider goroutine drain; the pool guard
then releases its slot exactly once on channel close.

`drainAndClose` also returns the connection to the pool for reuse (the upstream
client in `main.go` is a single tuned `*http.Client`), satisfying the NFR-004
timeout/pooling boundary.

## 16. Not determinable from code

- **NFR-001 (≤ 20 ms p95 overhead)** is the design intent stated in the model and
  ADR-0006; the actual measured overhead is not derivable from source (it depends
  on a benchmark identifier in `trace.yml`, not run here).
- **Real-backend behaviour** (Ollama/OpenAI latency, model availability, whether a
  given backend honours `stream_options.include_usage`) is environment-dependent
  and not asserted in CI; ADR-0013 states the Ollama profile is an optional,
  documented demo path, not a CI gate.
- v1 ships a **single registered upstream**; multi-provider routing by model
  prefix and the ADR-0006 "fallback provider on `ErrOpenState`" behaviour are
  explicit non-goals and are **not** implemented — on an open breaker the gateway
  fast-fails 503.
