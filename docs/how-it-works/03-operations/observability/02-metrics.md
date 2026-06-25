# 02 — Metrics

> Component **COMP-013** · code `internal/metrics/**.go`, exposed via `internal/server/server.go` · capability CAP-004 · FR-010, NFR-007 (6/6 mandatory metrics) · [ADR-0008](../../../../meta/architecture/decisions/adr/0008-observability-shared-prometheus-registry.md).

## Why an injected registry, not the global default

`prometheus/client_golang` ships a process-global `DefaultRegisterer`, and the path of
least resistance is to register metrics against it and serve `promhttp.Handler()`.
[ADR-0008](../../../../meta/architecture/decisions/adr/0008-observability-shared-prometheus-registry.md)
deliberately rejects that. The driving reason is **test isolation**: a global registry
is shared mutable state, so two tests (or two `metrics.New` calls) that register the
same metric name collide with a `duplicate metrics registration` panic, and metrics
leak between test cases. By constructing an explicit `prometheus.NewRegistry()` and
injecting it, every process — and every test — owns its own metric set, and parallel
tests can never interfere.

The package documents this contract up front (`internal/metrics/metrics.go`):

```go
// Per ADR-0008 the *prometheus.Registry is INJECTED — this package never touches
// prometheus.DefaultRegisterer and holds no package-level global. Each process
// (and each test) constructs its own registry via prometheus.NewRegistry() and
// passes it to New ...
```

The registry is created once in `cmd/gateway/main.go` and threaded everywhere by DI:

```go
promRegistry := prometheus.NewRegistry()
met := metrics.New(promRegistry)
```

## 1. Registration: `promauto.With(reg)`

`metrics.New` ties every metric to the injected registry through `promauto.With(reg)`
— the factory variant that registers against a *specific* registry instead of the
global default. From `internal/metrics/metrics.go`:

```go
func New(reg *prometheus.Registry) *Metrics {
	factory := promauto.With(reg)
	return &Metrics{
		reg: reg,
		HTTPRequestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of completed HTTP requests by route and status.",
		}, []string{"route", "status"}),
		HTTPRequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency in seconds by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		// ... (remaining six below)
	}
}
```

`promauto` registers eagerly during construction, so registering the same name twice
against one registry panics — the intended fail-fast for a duplicate-wiring bug.

## 2. The metric catalog (NFR-007)

`metrics.New` registers **eight** series. The six in the C4 model (NFR-007 mandatory
set) plus two metering series (COMP-016). Names, labels, types and meaning are taken
verbatim from `internal/metrics/metrics.go`:

| Metric | Type | Labels | Meaning | Emitted from |
|---|---|---|---|---|
| `http_requests_total` | CounterVec | `route`, `status` | Completed HTTP requests | `(*Metrics).Middleware` (`internal/metrics/middleware.go`) |
| `http_request_duration_seconds` | HistogramVec (`DefBuckets`) | `route` | HTTP request latency, seconds | `(*Metrics).Middleware` |
| `gateway_inflight_requests` | Gauge | — | Requests currently being served | `(*Metrics).Middleware` (Inc/Dec) |
| `provider_request_duration_seconds` | HistogramVec (`DefBuckets`) | `provider` | Upstream-call latency, seconds | `InstrumentInferFunc` / `InstrumentStreamFunc` (`internal/metrics/infer.go`) |
| `ratelimit_rejected_total` | Counter | — | 429s from the rate limiter | rate-limit middleware via `Recorder.IncRateLimitRejected` |
| `breaker_state` | GaugeVec | `provider` | Circuit-breaker state: `0` closed, `1` half-open, `2` open | breaker via `Recorder.SetBreakerState` |
| `metering_events_dropped_total` | Counter | — | Usage events dropped on a full metering buffer | metering via `Recorder.IncMeteringEventsDropped` |
| `metering_buffer_size` | Gauge | — | Current usage-buffer occupancy | metering worker via `Recorder.SetMeteringBufferSize` |

The `breaker_state` numeric encoding is fixed by stable constants so the gauge is
queryable without string labels:

```go
const (
	BreakerStateClosed   = 0.0
	BreakerStateHalfOpen = 1.0
	BreakerStateOpen     = 2.0
)
```

### The `Recorder` port keeps Prometheus out of the resilience/metering packages

The rate-limit middleware, the circuit breaker and the metering worker emit metrics
without importing Prometheus. `metrics.go` defines a narrow `Recorder` interface
exposing only the signals those packages produce; `*Metrics` satisfies it, and a
`NopRecorder` lets call sites stay un-instrumented in tests:

```go
type Recorder interface {
	IncRateLimitRejected()
	SetBreakerState(provider string, state float64)
	IncMeteringEventsDropped()
	SetMeteringBufferSize(n int)
}
```

This is the boundary hygiene ADR-0008 calls for: those contexts depend on the small
port, not on `*Metrics` or any `prometheus` type, so the Prometheus dependency does
not leak across the ratelimit/breaker/metering boundaries.

## 3. Where HTTP metrics are incremented — and how cardinality is controlled

`(*Metrics).Middleware` instruments every HTTP request. It increments inflight before
the handler, times the request, and records the outcome in a `defer` so the numbers
are correct even on panic. From `internal/metrics/middleware.go`:

```go
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		m.InflightRequests.Inc()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			// Read r.Pattern AFTER next.ServeHTTP so the ServeMux has set it to
			// the matched route template. An empty Pattern means no route matched
			// (404); bucket all such requests under the fixed label "other" to
			// prevent unbounded Prometheus series from raw URL paths.
			route := r.Pattern
			if route == "" {
				route = "other"
			}
			m.InflightRequests.Dec()
			m.HTTPRequestDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
			m.HTTPRequestsTotal.WithLabelValues(route, strconv.Itoa(rec.status)).Inc()
		}()

		next.ServeHTTP(rec, r)
	})
}
```

The **cardinality control** is the key detail: the `route` label is the *matched
ServeMux pattern* (`r.Pattern`, populated by Go 1.22+ net/http *after* routing), not
the raw URL. An unmatched path (404) would otherwise let an attacker mint an unbounded
number of label values from arbitrary URLs, so every unmatched request is bucketed
under the single fixed label `"other"`. `gateway_inflight_requests` is decremented in
the same `defer`, so it stays accurate even when the handler panics (the recovery
middleware then turns that into a 500).

### Provider-call metrics on the InferFunc seam

`provider_request_duration_seconds` is recorded by decorating the provider-call seam,
*outside* the resilience layer, so the timing reflects the full resilient call
(including retries) — the latency a client actually experiences
(`internal/metrics/infer.go`):

```go
func (m *Metrics) InstrumentInferFunc(next server.InferFunc) server.InferFunc {
	return func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
		start := time.Now()
		resp, err := next(ctx, p, req)
		m.ProviderRequestDuration.WithLabelValues(req.Model).Observe(time.Since(start).Seconds())
		return resp, err
	}
}
```

The duration is observed regardless of success or failure. `InstrumentStreamFunc` gives
the streaming path the same coverage, but times the **full stream lifetime** — the
observation fires when the forwarded channel closes (normal end, ctx cancel, or error),
not when the channel is first returned. The `provider` label is `req.Model`, matching
the per-provider breaker keying.

## 4. Exposition: `GET /metrics`

The endpoint serves the *injected* registry via `promhttp.HandlerFor` (per ADR-0008,
**not** `promhttp.Handler()`, which would expose the global default). From
`internal/server/server.go`:

```go
func (r metricsResponse) VisitGetMetricsResponse(w http.ResponseWriter) error {
	h := promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
	h.ServeHTTP(w, &http.Request{Method: http.MethodGet, Header: make(http.Header)})
	return nil
}
```

`GetMetrics` falls back to an empty `200` body when no registry is injected, so the
generated OpenAPI contract stays satisfied even without metrics wiring.

### Seeding series so all six emit on startup

A Prometheus `*Vec` series does not exist until a label combination is first observed.
To guarantee AC-048 (all six mandatory metrics present on a freshly-started gateway,
even before any traffic), `cmd/gateway/main.go` seeds the breaker gauge to closed for
the registered provider at boot:

```go
// Seed breaker_state{provider} to closed (0) at registration so the series
// is always present in GET /metrics, even for a provider that has never
// tripped its breaker (AC-048: all six metrics must emit on startup).
met.SetBreakerState("mock", metrics.BreakerStateClosed)
```

See [diagrams/01-instrumentation-flow.puml](diagrams/01-instrumentation-flow.puml) for
how each metric is emitted along the request path.

## Related docs

- Decision record: [ADR-0008 — shared injected Prometheus registry](../../../../meta/architecture/decisions/adr/0008-observability-shared-prometheus-registry.md)
- Operator runbook: [`docs/role/operator/monitoring-and-metrics.md`](../../../role/operator/monitoring-and-metrics.md)
- Metrics emitted by other contexts:
  [`../../01-surface-api/proxy/`](../../01-surface-api/proxy/) (HTTP + inflight),
  [`../../02-resilience/resilience/`](../../02-resilience/resilience/) (`ratelimit_rejected_total`, `breaker_state`),
  [`../../04-integrations/metering/`](../../04-integrations/metering/) (`metering_events_dropped_total`, `metering_buffer_size`)
- [01 — Health and readiness](01-health-and-readiness.md) · [03 — Tracing and logging](03-tracing-and-logging.md)
