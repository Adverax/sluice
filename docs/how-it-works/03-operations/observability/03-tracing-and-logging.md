# 03 — Tracing and logging

> Components **COMP-014** (tracing, `internal/tracing/**.go` + `internal/middleware/tracing.go`) and **COMP-015** (logging, `internal/logging/**.go`) · capability CAP-004 · FR-011 (traces), FR-016 (structured logs), AC-030 (≥2-span trace), AC-050 (collector-down tolerance), AC-040/AC-041 (per-request + panic logging).

## Why tracing must never block a request

Distributed tracing is a *diagnostic* signal — it must never become a *failure* mode.
If exporting a span to the collector were on the request's critical path, a slow or
unreachable collector would stall every request. `sluice` decouples export from the
request entirely (AC-050), and degrades to running un-traced rather than failing to
boot. The tracing package states the contract up front (`internal/tracing/tracing.go`):

```go
// Collector-down tolerance (AC-050): the SDK uses a BATCH span processor, which
// exports asynchronously on a background goroutine. Export failures (collector
// unreachable, network errors) are therefore decoupled from the request path ...
// If the endpoint is unset, a no-op provider is returned so the gateway runs
// un-traced rather than failing to boot.
```

## 1. Tracer setup: batch export, fail-open

`tracing.New` builds an OTLP/HTTP exporter and wraps it in a **batch** span processor.
Two things make it fail-open. First, exporter or resource construction errors are
logged at WARN and downgraded to a disabled (no-op) provider with a `nil` error —
tracing never blocks startup. Second, an empty endpoint short-circuits to the no-op
provider. From `internal/tracing/tracing.go`:

```go
func New(ctx context.Context, cfg Config, logger *slog.Logger) *Provider {
	if cfg.Endpoint == "" {
		logger.LogAttrs(ctx, slog.LevelInfo, "tracing disabled (no OTLP endpoint configured)")
		return disabled()
	}
	// ...
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "tracing exporter init failed; continuing without tracing",
			slog.String("error", err.Error()))
		return disabled()
	}
	// ...
	tp := sdktrace.NewTracerProvider(
		// Batch (async) processor: export failures happen off the request path so
		// a down collector can never interrupt request processing (AC-050).
		sdktrace.WithBatcher(exporter, sdktrace.WithExportTimeout(exportTimeout)),
		sdktrace.WithResource(res),
	)
	return &Provider{tp: tp, shutdown: tp.Shutdown}
}
```

Note `otlptracehttp.New` only *builds* the client — it does not dial the collector —
so an unreachable collector cannot fail startup; the first export happens lazily on the
batch processor's background goroutine. The `disabled()` path returns a
`noop.NewTracerProvider()`, so call sites never need a nil check. The `Provider` is
injected into the HTTP middleware; the package never calls `otel.SetTracerProvider`
(no globals — ADR-0008). On graceful shutdown the gateway calls `Provider.Shutdown` to
flush pending spans; its error is logged but never affects requests.

The endpoint comes from `GATEWAY_OTEL_ENDPOINT` (`cmd/gateway/main.go`):

```go
tracer := tracing.New(context.Background(), tracing.Config{
	Endpoint: os.Getenv("GATEWAY_OTEL_ENDPOINT"),
	Insecure: true,
}, logger)
```

> **Transport — code says OTLP/HTTP, not gRPC.** The exporter is
> `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` — i.e. spans are
> exported over **OTLP/HTTP**. The C4 component diagram
> (`c4/components-observability.puml`) and `contexts.yml` describe the collector link
> as "gRPC"; the shipped code uses HTTP. The endpoint is whatever `GATEWAY_OTEL_ENDPOINT`
> is set to (the package doc gives `otel-collector:4318` as the example host:port, the
> conventional OTLP/HTTP port), with `Insecure: true` (plain HTTP, no TLS) on the
> running gateway. **Not determinable from code:** the exact deployed endpoint/port —
> it is supplied entirely via the environment variable.

## 2. The two-span trace (AC-030)

A meaningful trace needs at least the incoming HTTP request *and* the upstream provider
call. The HTTP middleware starts the **root** span; the instrumented `InferFunc` starts
a **child** span via the propagated context.

The HTTP middleware (`internal/middleware/tracing.go`) starts a low-cardinality span
*before* routing, then renames it to the matched route once known:

```go
func Tracing(tracer trace.Tracer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, span := tracer.Start(r.Context(), "HTTP "+r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.target", r.URL.RequestURI()),
				),
			)
			defer span.End()

			rec := &tracingStatusRecorder{ResponseWriter: w, status: http.StatusOK}
			r2 := r.WithContext(ctx)
			next.ServeHTTP(rec, r2)

			route := r2.Pattern
			if route == "" {
				route = "other"
			}
			span.SetName("HTTP " + r.Method + " " + route)
			span.SetAttributes(
				attribute.String("http.route", route),
				attribute.Int("http.status_code", rec.status),
			)
			if rec.status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}
```

The same cardinality discipline as the metrics middleware applies: the raw path may be
carried as the `http.target` *attribute* (high-cardinality is acceptable there), but the
span **name** and the `http.route` attribute use only the matched route template, with
unmatched paths bucketed under `"other"`.

The child span is created by `(*Provider).InstrumentInferFunc`
(`internal/tracing/infer.go`), which starts `"provider.infer"` from the context that
already carries the root span — establishing the parent/child link:

```go
func (p *Provider) InstrumentInferFunc(next server.InferFunc) server.InferFunc {
	tracer := p.Tracer()
	return func(ctx context.Context, prov provider.Provider, req provider.Request) (provider.Response, error) {
		ctx, span := tracer.Start(ctx, "provider.infer")
		span.SetAttributes(attribute.String("provider.model", req.Model))
		defer span.End()

		resp, err := next(ctx, prov, req)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		return resp, err
	}
}
```

`InstrumentStreamFunc` mirrors this for streaming under the span name `"provider.stream"`,
ending the span when the forwarded channel closes so trace duration reflects the whole
stream. The decorators are composed so tracing wraps metrics on the seam
(`cmd/gateway/main.go`):

```go
instrumentedInfer := tracer.InstrumentInferFunc(met.InstrumentInferFunc(guardedInfer))
```

See [diagrams/01-instrumentation-flow.puml](diagrams/01-instrumentation-flow.puml).

## 3. Structured logging per request

The logger is a plain `*slog.Logger`, constructed once and injected (no globals,
ADR-0008). `logging.New` picks JSON (production) or text (local dev) by format string,
and parses the level (`internal/logging/logging.go`); `cmd/gateway/main.go` writes it to
`os.Stdout`.

The per-request middleware (`internal/logging/middleware.go`) does three jobs:
generate/propagate a request id, time the request, and emit one INFO line on completion.

```go
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" {
				requestID = newRequestID()
			}
			ctx := context.WithValue(r.Context(), requestIDKey, requestID)
			r = r.WithContext(ctx)
			w.Header().Set("X-Request-ID", requestID)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			defer func() {
				latencyMS := time.Since(start).Milliseconds()
				if rv := recover(); rv != nil {
					LogPanic(ctx, logger, rv,
						slog.String("request_id", requestID),
						slog.Int64("latency_ms", latencyMS),
					)
					panic(rv) // re-panic so the recovery middleware emits the 500
				}
				logger.LogAttrs(ctx, slog.LevelInfo, "request completed",
					slog.String("request_id", requestID),
					slog.Int64("latency_ms", latencyMS),
					slog.Int("status_code", rec.status),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}
```

Operationally relevant behavior:

- **Request id is inbound-respecting.** An incoming `X-Request-ID` header is reused;
  otherwise a random 128-bit hex id is minted by `newRequestID` (falling back to a
  timestamp only if the system RNG fails, so a request is never left without an id). The
  id is stored on the context (readable via `RequestIDFromContext`) *and* echoed back in
  the `X-Request-ID` response header, so it ties client, gateway log, and trace together.
- **One INFO line per completed request** carrying `request_id`, `latency_ms`,
  `status_code`, `method`, `path` (FR-016 / AC-040).
- **Panics are logged, then re-raised.** On a recovered panic the middleware logs at
  ERROR (suppressing the normal completion line for that request) and re-panics so the
  outer recovery middleware can produce the 500.

## 4. Panic logging: one contract, two call sites

`LogPanic` is the single panic-logging contract (AC-041) — `ERROR` level, a
`panic_value` field, plus any extra attributes (`internal/logging/middleware.go`):

```go
func LogPanic(ctx context.Context, logger *slog.Logger, panicValue any, attrs ...slog.Attr) {
	all := append([]slog.Attr{slog.Any("panic_value", panicValue)}, attrs...)
	logger.LogAttrs(ctx, slog.LevelError, "panic recovered", all...)
}
```

Both the logging middleware (above) and the recovery middleware reuse it, so the panic
log contract is identical everywhere. The recovery middleware
(`internal/middleware/recover.go`) is installed as the **outermost** layer: it performs
the final `recover()`, calls `LogPanic` with `request_id` and a full `debug.Stack()`,
and writes the 500 — while letting `http.ErrAbortHandler` pass through untouched so
net/http can abort a hijacked/flushed connection as intended. `SafeGo` in the same file
applies the same `LogPanic` contract to detached goroutines, which a request-handler
`defer` cannot otherwise recover.

The composition order is fixed in `cmd/gateway/main.go` — recovery outermost, then
logging, then tracing, then metrics:

```go
handler := middleware.Recoverer(logger)(
	logging.Middleware(logger)(
		middleware.Tracing(tracer.Tracer())(
			met.Middleware(
				// ... rate-limit, counting, cache, app handler
```

## Related docs

- Decision record: [ADR-0008 — injected `slog.Logger`, no globals](../../../../meta/architecture/decisions/adr/0008-observability-shared-prometheus-registry.md)
- Operator runbook: [`docs/role/operator/monitoring-and-metrics.md`](../../../role/operator/monitoring-and-metrics.md)
- Panic recovery / resilience: [`../../02-resilience/resilience/`](../../02-resilience/resilience/)
- Request path the spans/logs wrap: [`../../01-surface-api/proxy/`](../../01-surface-api/proxy/)
- [01 — Health and readiness](01-health-and-readiness.md) · [02 — Metrics](02-metrics.md)
