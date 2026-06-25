# internal/middleware

`net/http` middleware for the gateway's request pipeline. Each middleware is a
`func(http.Handler) http.Handler` composed in `cmd/gateway` (outermost first):

```
recover → logging → tracing → metrics → rate-limit → counting → routes
```

## Recoverer (COMP-007, FR-013)

`recover.go` — outermost middleware. A deferred `recover()` catches any panic from
a downstream handler, logs it at ERROR via `logging.LogPanic` (preserving the
`panic_value` field — AC-041), writes a JSON 500 to the client (AC-033), and lets
the process continue serving. `http.ErrAbortHandler` is re-raised unchanged so
`net/http`'s deliberate abort path is not masked.

`SafeGo(logger, fn)` — a `go fn()` replacement that installs its own `recover` so a
panic inside a handler-detached goroutine cannot crash the process (AC-034). Use
instead of a bare `go` statement whenever a handler spawns background work.

> `recover()` only catches panics on the same goroutine. Handler goroutines that
> spawn additional goroutines with bare `go` bypass Recoverer; those must use SafeGo.

## Tracing (COMP-014, FR-011)

`tracing.go` — creates the OTel root span for each request. Span name starts as
`"HTTP <method>"` (low cardinality) and is updated to `"HTTP <method> <r.Pattern>"`
after routing resolves the matched pattern. Unmatched paths are bucketed as `"other"`
to prevent unbounded span names. The span context is stored on the request context so
the provider-call decorator (in `internal/tracing`) becomes a child span (AC-030).

`tracingStatusRecorder` wraps `http.ResponseWriter` to capture the status code for
the span attribute. It implements `Unwrap() http.ResponseWriter` so
`http.ResponseController` can reach through to `Flusher`/`Hijacker` on the base
writer (required for CARD-004 SSE streaming).

## RateLimiter (COMP-008, CARD-005)

`ratelimit.go` — per-API-key rate limiting (FR-004, ADR-0001/0006). Runs after
tracing and metrics so 429s are still counted, but before any provider/pool work
(INV-004). Reports rejections via the injected `metrics.Recorder` without importing
Prometheus (ADR-0008).

- **Key resolution** (precedence): `Authorization` header → a well-formed
  `sluice_api_key` cookie (`eph_` + 32 hex) → mint fresh via `crypto/rand`.
- **Ephemeral minting**: keyless request → mint, return via `X-Sluice-Api-Key`
  header + HttpOnly `Set-Cookie`; the cookie round-trips so the next request reuses
  the same bucket.
- **Fail-closed** on `crypto/rand` error → 500.
- **Over limit** → 429 + `Retry-After`, next handler not called.

Backing limiter tiers and the distributed ACL live in `internal/ratelimit`.

## ResponseWriter wrappers

Both `tracingStatusRecorder` (tracing.go) and `statusRecorder` (metrics middleware in
`internal/metrics`) implement `Unwrap() http.ResponseWriter`. This allows
`http.ResponseController` — and `net/http`'s interface-capability detection — to
reach the base `ResponseWriter` through the wrapper chain, preserving `Flusher` and
`Hijacker` for SSE and WebSocket handlers (CARD-004) without forwarding each interface
individually.
