# internal/tracing

## Purpose

Implements COMP-014: OpenTelemetry SDK initialisation and provider-call span
decoration (FR-011). Provides a `Provider` that is always non-nil — either a real
OTLP/HTTP-backed tracer or a no-op — so call sites never need nil checks and
tracing never blocks the gateway from starting or serving (AC-050).

## Architecture

The OTel SDK is set up with a **batch span processor** whose export goroutine runs
entirely off the request path. A down or unreachable collector therefore cannot
delay, fail, or 429 requests — the span is enqueued in memory and the request
returns immediately (AC-050). Init and export errors are logged at WARN and
downgrade to the no-op provider; they are never propagated.

```
GATEWAY_OTEL_ENDPOINT (env)
  └─ tracing.New(ctx, Config{...}, logger)
       ├─ endpoint set   → OTLP/HTTP exporter (batch, async)
       │                   → real sdktrace.TracerProvider
       └─ endpoint unset → noop.NewTracerProvider()  (no boot failure)

Provider
  ├─ Tracer()                  → trace.Tracer (used by middleware.Tracing)
  ├─ Shutdown(ctx)             → flush batch processor on graceful drain
  └─ InstrumentInferFunc(next) → "provider.infer" child span decorator
```

Per ADR-0008 the provider is injected; this package never calls
`otel.SetTracerProvider`.

## Key types

| Type | Description |
|------|-------------|
| `Provider` | Wraps `trace.TracerProvider` + `Shutdown` hook. Always non-nil. |
| `Config` | `Endpoint` (host:port), `Insecure` (plain HTTP), `ExportTimeout`. |

## Usage

```go
// cmd/gateway startup
tracer := tracing.New(ctx, tracing.Config{
    Endpoint: os.Getenv("GATEWAY_OTEL_ENDPOINT"),
    Insecure: true,
}, logger)
defer tracer.Shutdown(shutdownCtx)  // flushes pending spans

// Middleware root span (see internal/middleware)
middleware.Tracing(tracer.Tracer())

// Provider-call child span decorator
instrumentedInfer = tracer.InstrumentInferFunc(met.InstrumentInferFunc(guardedInfer))
```

Tests inject a pre-built `trace.TracerProvider` (e.g. `tracetest.NewTracerProvider`)
via `NewWithProvider(tp, shutdown)`, bypassing real OTLP wiring entirely.

## See also

- `internal/middleware` — `Tracing` middleware that starts the per-request root span
- `cmd/gateway` — wiring: `GATEWAY_OTEL_ENDPOINT`, shutdown hook, `InstrumentInferFunc` composition
- AC-050 — collector-down tolerance requirement
