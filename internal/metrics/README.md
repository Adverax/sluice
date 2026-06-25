# internal/metrics

## Purpose

Implements COMP-013: the Prometheus metrics registry and the six required gateway
metrics (NFR-007 / AC-048). Per ADR-0008 the `*prometheus.Registry` is always
injected — this package never touches `prometheus.DefaultRegisterer` and holds no
package-level globals, so parallel tests cannot collide.

## Architecture

All six metrics are registered against the caller-supplied registry via
`promauto.With(reg)`. The `Recorder` interface is the only surface exposed to
`internal/ratelimit` and `internal/breaker`, keeping the Prometheus import
confined to this package (ADR-0008 boundary hygiene).

```
cmd/gateway
  └─ metrics.New(prometheus.NewRegistry())
       ├─ *Metrics implements Recorder  ───► ratelimit middleware
       │                                ───► breaker registry
       ├─ Metrics.Middleware(next)      ───► middleware chain (HTTP metrics)
       └─ Metrics.InstrumentInferFunc   ───► InferFunc decorator (provider latency)
```

## Key types

| Type | Description |
|------|-------------|
| `Metrics` | Holds the six metric handles; constructed via `New(reg)`. |
| `Recorder` | Narrow port: `IncRateLimitRejected()` + `SetBreakerState(provider, state)`. Implemented by `*Metrics` and `NopRecorder`. |
| `NopRecorder` | Discards all signals; used in tests that don't need instrumentation. |

## Metrics exposed

| Metric | Type | Labels |
|--------|------|--------|
| `http_requests_total` | counter | `route`, `status` |
| `http_request_duration_seconds` | histogram | `route` |
| `gateway_inflight_requests` | gauge | — |
| `provider_request_duration_seconds` | histogram | `provider` |
| `ratelimit_rejected_total` | counter | — |
| `breaker_state` | gauge | `provider` (0=closed, 1=half-open, 2=open) |

`breaker_state` is seeded to `0` at provider registration in `cmd/gateway` so the
series is present in `/metrics` even before any state transition (AC-048).

## Usage

```go
reg := prometheus.NewRegistry()
met := metrics.New(reg)

// HTTP middleware — reads r.Pattern after routing; unmatched paths → "other"
handler = met.Middleware(handler)

// Provider-latency decorator — wraps the composed InferFunc
instrumentedInfer = met.InstrumentInferFunc(guardedInfer)

// Exposition — serve the injected registry (not the global one)
http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
```

Each test constructs a fresh `prometheus.NewRegistry()` to avoid cross-test
metric-name collisions.

## See also

- `internal/middleware` — HTTP middleware chain that calls `met.Middleware`
- `cmd/gateway` — wiring: registry injection, `/metrics` handler, `breaker_state` seeding
- ADR-0008 — dependency-injection rule (no global registerers)
