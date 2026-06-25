# CAP-001 — Inference proxying

> Source of truth: `cmd/gateway/main.go`, `internal/middleware/cache.go`,
> `internal/server/server.go`, `internal/proxy/router.go`,
> `internal/proxy/resilience/resilience.go`, `internal/proxy/retry/retry.go`,
> `internal/provider/provider.go`, `internal/provider/httpprovider.go`.

## 1. Why this exists

sluice is an LLM gateway: it sits between API clients and an upstream provider
and turns a single client request into a routed, resilient, observable,
optionally-cached upstream call. CAP-001 is that hot path. The design goal
(NFR-001) is to add **≤ 20 ms p95 overhead** excluding provider latency, so
every layer on the path is either a cheap header/body check or a non-blocking
hand-off. The shape of the path is fixed by **ADR-0006** (the request-composition
order) and **ADR-0009** (one `Provider` interface as the anti-corruption layer);
the HTTP boundary is **generated** from the OpenAPI contract (ADR-0011), so the
Go code in this context only owns the behaviour *behind* that contract.

A request crosses, in order: the **middleware chain** → the **cache** → the
**handler** (`Server.CreateChatCompletion`) → the **router** (model → provider) →
the **resilience seam** (`InferFunc`/`StreamFunc`) → the **provider adapter** →
the upstream. See [diagrams/01-inference-proxying-01.puml](diagrams/01-inference-proxying-01.puml)
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

The order is the one mandated by ADR-0006 and documented inline in `main.go`:

> `recover → logging → tracing → metrics → rate-limit → counting → cache → routes`

Why this order matters for CAP-001:

- **Panic recovery is outermost** so any handler panic becomes a 500 and the
  process survives (CAP-005, file `02-runtime-lifecycle.md`).
- **Rate-limit runs before any provider/pool work** (INV-004): a 429 never
  reaches the proxy core. Rate limiting lives in the
  [Resilience](../../02-resilience/resilience/) context; ADR-0006 puts it in the
  chain because the API key is in the `Authorization` header, available *before*
  the body is parsed.
- **Counting middleware** (`manager.CountingMiddleware`) increments the in-flight
  gauge that graceful shutdown drains (CAP-005).
- **Cache is innermost**, just before the generated routes, so a cache HIT
  short-circuits the provider path while still being wrapped by the outer
  logging/metrics/tracing instrumentation.

## 3. Cache lookup (FR-005, ADR-0004 / ADR-0010)

`CacheMiddleware.Middleware` (`internal/middleware/cache.go`) acts on exactly one
route — `POST /v1/chat/completions`. Everything else passes straight through:

```go
if m.repo == nil || r.Method != cacheMethod || r.URL.Path != cacheRoute {
    next.ServeHTTP(w, r)
    return
}
```

**Key.** The cache key is a hex sha256 of the request identity — method, path,
and the raw body bytes (NUL-separated):

```go
func cacheKey(method, path string, body []byte) string {
    h := sha256.New()
    h.Write([]byte(method)); h.Write([]byte{0})
    h.Write([]byte(path));   h.Write([]byte{0})
    h.Write(body)
    return hex.EncodeToString(h.Sum(nil))
}
```

The body is read with a `LimitReader` capped at `maxBodyBytes` (default 1 MiB)
and then **restored** (`r.Body = io.NopCloser(bytes.NewReader(body))`) so the
downstream handler can re-read it. Oversize bodies are reconstructed in full and
bypass the cache — never a 413 from this layer. The key intentionally does **not**
include the API key: identical request bodies produce identical completions, so
entries are shared cross-tenant by design (documented trade-off in the source).

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
is decoded and replayed with `X-Cache: HIT`. On a MISS the response is set to
`X-Cache: MISS` and captured by a `cacheRecorder` that streams bytes to the
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

## 4. Routing by model (FR-002)

`Server.CreateChatCompletion` (`internal/server/server.go`) runs behind the
generated strict server, which has already decoded the JSON body. The handler
validates and routes:

```go
if body.Model == "" {           // AC-006
    return badRequest("missing_model", "the 'model' field is required"), nil
}
if len(body.Messages) == 0 {
    return badRequest("empty_messages", "...at least one message"), nil
}
prov, err := s.router.Provider(body.Model)
if err != nil {
    if errors.Is(err, proxy.ErrModelNotRegistered) {
        return notFound("unknown_model", "no provider is registered for model "+body.Model), nil
    }
    ...
}
```

The `Router` (`internal/proxy/router.go`) is a small concurrency-safe registry —
`map[string]provider.Provider` behind an `RWMutex`. `Register` happens at startup
(`router.Register("mock", provider.NewHTTP(...))` in `main.go`); `Provider` runs
on every request and returns `ErrModelNotRegistered` for an unknown model, which
the handler maps to **404**.

## 5. Crossing the anti-corruption layer (ADR-0009)

The handler maps the generated DTO onto the canonical `provider.Request` with
`toCanonicalRequest` (e.g. widening `float32` temperature to `*float64`), calls
the provider, and maps the canonical `provider.Response` back with
`toAPIResponse`. No provider-specific type ever crosses the boundary. The
`Provider` interface is deliberately two methods, one per path
(`internal/provider/provider.go`):

```go
type Provider interface {
    Infer(ctx context.Context, req Request) (Response, error)
    InferStream(ctx context.Context, req Request) (<-chan Chunk, error)
}
```

The canonical `Usage{PromptTokens, CompletionTokens, TotalTokens}` is what
metering reads (Section 9), so the metering context never imports a provider
package. Upstream non-2xx statuses become a `*StatusError` whose `Retryable()`
returns true only for 5xx — this is how the retry/breaker layer classifies
failures without string-matching.

## 6. The resilience seam (`InferFunc` / `StreamFunc`, ADR-0006)

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
unary seam as `retry(breaker.Execute(providerCall))`, keyed by the request model:

```go
func (c *Composer) InferFunc() server.InferFunc {
    return func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
        key := req.Model
        guarded := func(ctx context.Context) (provider.Response, error) {
            return c.breakers.Execute(ctx, key, func(ctx context.Context) (provider.Response, error) {
                return p.Infer(ctx, req)
            })
        }
        resp, err := c.retrier.Do(ctx, guarded)
        ...
    }
}
```

The breaker, retry engine, and worker pool themselves live in the
[Resilience](../../02-resilience/resilience/) context; this aspect documents only
how the proxy *composes and consumes* them. The error mapping the seam produces
(see Section 8) is what makes the proxy's 503/502 behaviour correct.

## 7. Retry with backoff on the path (FR-006)

The Retry Engine (`internal/proxy/retry/retry.go`) wraps a generic
`Call func(ctx) (provider.Response, error)` — the same shape the breaker wraps,
which is what lets them compose. `Do` is bounded, deadline-aware, and classifies
errors by type, not string:

```go
for attempt := 1; attempt <= attempts; attempt++ {
    if err := ctx.Err(); err != nil { /* never start an attempt against a dead ctx (AC-020) */ }
    resp, err := call(ctx)
    if err == nil { return resp, nil }
    lastErr = err
    if !e.shouldRetry(ctx, err) { return provider.Response{}, err }   // 4xx / ctx / ErrOpenState → immediate
    if attempt == attempts { break }
    if serr := e.sleep(ctx, e.backoff(attempt)); serr != nil { ... }  // ctx-aware backoff
}
return provider.Response{}, fmt.Errorf("%w after %d attempt(s): %w", ErrExhausted, attempts, lastErr)
```

`shouldRetry` returns false for the composition root's non-retryable predicate
(an **open breaker** — `resilience.IsOpenState`, wired via
`retry.WithNonRetryable` in `main.go`), for context cancellation/deadline, and
for a 4xx `StatusError`; a 5xx `StatusError` is retryable, and an unclassified
error is treated as transient. `backoff` is `BaseDelay * 2^(attempt-1)` capped at
`MaxDelay` with up to `Jitter` fraction of randomised reduction. Exhausting the
budget wraps the last error in `ErrExhausted`, which the handler maps to 502.

The "don't retry into an open breaker" rule (ADR-0006) is enforced because
`breaker.ErrOpenState` is marked non-retryable, so the retry layer propagates it
unwrapped instead of spinning.

## 8. Error mapping (503 / 502 / 429 / 400 / 404)

The seam classifies the failure; the handler maps it to HTTP:

| Cause | Seam result | Handler → HTTP |
|-------|-------------|----------------|
| Open breaker | `*Unavailable{Reason:"breaker_open"}` (matches `errors.Is(err, server.ErrServiceUnavailable)`) | **503** + `Retry-After` |
| Client deadline/cancel during retry | `*Unavailable{Reason:"deadline"}` | **503** + `Retry-After` |
| Worker pool saturated | `pool.ErrPoolSaturated` (also matches `ErrServiceUnavailable`, carries `RetryAfter()`) | **503** + `Retry-After` |
| 5xx upstream, retries exhausted | wrapped `retry.ErrExhausted` | **502** `provider_error` |
| 4xx `StatusError` | returned unwrapped (not retried) | **502** `provider_error` |
| Missing / empty model / body | — (validated in handler) | **400** |
| Unknown model | `proxy.ErrModelNotRegistered` | **404** |
| Rate limit exceeded | (rate-limit middleware, before the proxy) | **429** + `Retry-After` |

The 503 check is intentionally **before** the generic 502 so resilience signals
are not masked as provider errors:

```go
resp, err := s.infer(ctx, prov, req)
if err != nil {
    if errors.Is(err, ErrServiceUnavailable) {     // open breaker / deadline / pool saturation
        return s.serviceUnavailable(err), nil       // 503 + Retry-After
    }
    return badGateway("provider_error", "upstream provider request failed"), nil  // 502
}
```

`ErrServiceUnavailable` is a sentinel defined in the `server` package (not the
resilience package) to avoid an import cycle: `resilience.Unavailable` implements
`Is(target) bool` returning `target == server.ErrServiceUnavailable`, and
`RetryAfter() time.Duration`. `serviceUnavailableResponse.VisitCreateChatCompletionResponse`
sets the `Retry-After` header (rounded to whole seconds, floored at 1) and then
emits the generated 503 JSON body.

## 9. Async usage metering on the success path (FR-014)

On a successful unary response the handler records a usage event before
returning:

```go
s.recordUsage(ctx, body.Model, resp.Model, resp.Usage, time.Since(start), http.StatusOK)
return api.CreateChatCompletion200JSONResponse(toAPIResponse(resp)), nil
```

`recordUsage` builds a `metering.UsageEvent` (provider alias, resolved model,
canonical token counts, latency, status, request-id from context) and calls
`s.meter.Enqueue`. The sink (`metering.Sink`) is **non-blocking by contract** —
on a full buffer the event is dropped (INV-003 / CON-006) — so metering never
delays the hot path. It defaults to `metering.NopSink{}` and is injected via
`WithMeteringSink(meteringBuffer)` in `main.go`. The downstream worker, batching,
and Postgres persistence are documented in
[Metering](../../04-integrations/metering/).

## 10. Non-streaming vs streaming response

**Non-streaming** is the path above: a buffered `provider.Response` mapped to a
`CreateChatCompletion200JSONResponse`.

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
    return r.writeInitError(w, err)   // 503 (open breaker / pool) or 502 (provider) JSON, no SSE bytes
}
w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-cache")
w.Header().Set("Connection", "keep-alive")
w.WriteHeader(http.StatusOK)
rc := http.NewResponseController(w)
_ = rc.Flush()   // flush headers so the client sees the stream open
```

The streaming seam (`composer.StreamFunc`) is `breaker.ExecuteStream(provider.InferStream)`
with **no retry** — a partially-sent stream cannot be safely replayed (documented
in ADR-0006 and in the seam's doc comment).

**Per-chunk flush.** The forwarding loop selects on `ctx.Done()` versus the chunk
channel, writes each chunk as an SSE `data:` event, and flushes after every
write:

```go
for {
    select {
    case <-r.ctx.Done():
        return nil   // client disconnect / deadline — stop forwarding
    case chunk, ok := <-ch:
        if !ok { _, _ = w.Write([]byte(sseDone)); _ = rc.Flush(); return nil }   // normal end
        if chunk.Err != nil { /* log; emit [DONE]; return */ }
        if chunk.Done { /* emit usage chunk + [DONE]; recordUsage; return */ }
        payload, _ := json.Marshal(toAPIChunk(chunk))
        if _, wErr := fmt.Fprintf(w, "data: %s\n\n", payload); wErr != nil { return nil }
        _ = rc.Flush()
    }
}
```

Two details are load-bearing:

- Flushing goes through **`http.NewResponseController(w).Flush()`**, not a
  `w.(http.Flusher)` assertion, because the metrics/tracing/cache middleware wrap
  the writer and implement `Unwrap() http.ResponseWriter`; the controller
  traverses that unwrap chain to reach the real flusher (`cacheRecorder.Unwrap`
  is exactly that hook).
- The stream terminates with the conventional marker
  `const sseDone = "data: [DONE]\n\n"` on every exit path.

See [diagrams/01-inference-proxying-02.puml](diagrams/01-inference-proxying-02.puml).

## 11. Client-cancellation aborts the upstream (FR-003, INV-002)

Cancellation is end-to-end via `context.Context`. The per-request `ctx` is
threaded into the `InferFunc`/`StreamFunc` and on into the provider. The
`HTTPProvider` (`internal/provider/httpprovider.go`) builds every request with
`http.NewRequestWithContext`, so a cancelled ctx aborts the in-flight upstream
HTTP call:

```go
httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
...
resp, err := p.client.Do(httpReq)
if err != nil { return Response{}, mapTransportError(err) }
```

`mapTransportError` surfaces `context.Canceled` / `context.DeadlineExceeded`
unwrapped so callers (and the retry classifier) can match them with `errors.Is`.
On the streaming side, `streamLoop` checks `ctx.Err()` between events and
`sendChunk` selects on `ctx.Done()`, then `defer drainAndClose(body)` /
`defer close(out)` ensure the upstream body is closed and the channel closed on
exit — no goroutine leak. When the client disconnects, the handler's
`case <-r.ctx.Done()` returns, which (because the same ctx was passed to the
stream) cancels the upstream and lets the provider goroutine drain; the pool
guard then releases its slot exactly once on channel close.

`drainAndClose` also returns the connection to the pool for reuse (the upstream
client in `main.go` is a single tuned `*http.Client` with
`MaxIdleConns/MaxIdleConnsPerHost = 100`), satisfying the NFR-004 timeout/pooling
boundary.

## 12. Not determinable from code

- **NFR-001 (≤ 20 ms p95 overhead)** is the design intent stated in the model and
  ADR-0006; the actual measured overhead is not derivable from source (it depends
  on the benchmark `BenchGateway_p95OverheadUnder20ms`, which is a planned
  identifier in `trace.yml`, not run here).
- v1 ships a **single mock provider** (`router.Register("mock", ...)`); the
  ADR-0006 "route to a fallback provider on `ErrOpenState`" behaviour is
  explicitly a documented extension and is **not** implemented — on an open
  breaker the gateway fast-fails 503.
