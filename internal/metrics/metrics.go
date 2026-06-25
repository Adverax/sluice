// Package metrics implements COMP-013, the Prometheus metrics registry and the
// six required gateway metrics (NFR-007 / AC-048). Per ADR-0008 the
// *prometheus.Registry is INJECTED — this package never touches
// prometheus.DefaultRegisterer and holds no package-level global. Each process
// (and each test) constructs its own registry via prometheus.NewRegistry() and
// passes it to New, so metric registration is isolated and collisions between
// parallel tests are impossible.
//
// Ports & adapters (forge:engineering-standards): the cross-cutting collaborators
// that must not import Prometheus — the rate-limit middleware (ADR-0001) and the
// circuit breaker (ADR-0002) — depend only on the small Recorder interface
// defined here, not on *Metrics or any prometheus type. This keeps the
// Prometheus dependency from leaking across the ratelimit/breaker boundaries.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Recorder is the narrow port the rate-limit middleware and the circuit breaker
// depend on. It exposes only the two signals those packages emit, so neither has
// to import Prometheus (ADR-0008 boundary hygiene). *Metrics satisfies it; a
// no-op implementation (NopRecorder) lets callers stay un-instrumented in tests.
type Recorder interface {
	// IncRateLimitRejected records a single 429 rejection at the rate-limit
	// boundary (ratelimit_rejected_total).
	IncRateLimitRejected()
	// SetBreakerState records the current circuit-breaker state for a provider
	// (breaker_state): 0=closed, 1=half-open, 2=open.
	SetBreakerState(provider string, state float64)
	// IncMeteringEventsDropped records a single usage event dropped by the
	// metering buffer when it is full (metering_events_dropped_total). The
	// metering package depends on the narrower metering.DropRecorder port, which
	// *Metrics also satisfies, so it never imports Prometheus (ADR-0008).
	IncMeteringEventsDropped()
}

// Breaker state gauge values (breaker_state). Stable numeric encoding so the
// gauge is queryable in Prometheus/Grafana without string labels.
const (
	BreakerStateClosed   = 0.0
	BreakerStateHalfOpen = 1.0
	BreakerStateOpen     = 2.0
)

// Metrics holds the six required gateway metrics, all registered against the
// injected registry via promauto.With(reg) (ADR-0008).
type Metrics struct {
	reg *prometheus.Registry

	// HTTPRequestsTotal counts completed HTTP requests by route and status
	// (http_requests_total{route,status}).
	HTTPRequestsTotal *prometheus.CounterVec
	// HTTPRequestDuration is the request-latency histogram in seconds
	// (http_request_duration_seconds{route}).
	HTTPRequestDuration *prometheus.HistogramVec
	// InflightRequests is the gauge of requests currently being served
	// (gateway_inflight_requests).
	InflightRequests prometheus.Gauge
	// ProviderRequestDuration is the upstream-call latency histogram in seconds
	// (provider_request_duration_seconds{provider}).
	ProviderRequestDuration *prometheus.HistogramVec
	// RateLimitRejected counts 429 rejections (ratelimit_rejected_total).
	RateLimitRejected prometheus.Counter
	// BreakerState is the per-provider circuit-breaker state gauge
	// (breaker_state{provider}).
	BreakerState *prometheus.GaugeVec
	// MeteringEventsDropped counts usage events dropped by the metering buffer
	// when it is full (metering_events_dropped_total, COMP-016 / AC-036).
	MeteringEventsDropped prometheus.Counter
}

// New registers the six required metrics against reg and returns the handle.
// reg MUST be non-nil and freshly constructed by the caller (ADR-0008); using
// promauto.With(reg) ties every metric to that registry rather than the global
// default. Registering twice against the same registry panics (promauto
// semantics), which is the desired fail-fast for a duplicate-wiring bug.
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
		InflightRequests: factory.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_inflight_requests",
			Help: "Number of HTTP requests currently being served.",
		}),
		ProviderRequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "provider_request_duration_seconds",
			Help:    "Upstream provider request latency in seconds by provider.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider"}),
		RateLimitRejected: factory.NewCounter(prometheus.CounterOpts{
			Name: "ratelimit_rejected_total",
			Help: "Total number of requests rejected (429) by the rate limiter.",
		}),
		BreakerState: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "breaker_state",
			Help: "Circuit-breaker state by provider (0=closed, 1=half-open, 2=open).",
		}, []string{"provider"}),
		MeteringEventsDropped: factory.NewCounter(prometheus.CounterOpts{
			Name: "metering_events_dropped_total",
			Help: "Total number of usage events dropped because the metering buffer was full.",
		}),
	}
}

// Registry returns the underlying injected registry so the exposition handler
// (GET /metrics) can be built from it.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// IncRateLimitRejected implements Recorder.
func (m *Metrics) IncRateLimitRejected() { m.RateLimitRejected.Inc() }

// SetBreakerState implements Recorder.
func (m *Metrics) SetBreakerState(provider string, state float64) {
	m.BreakerState.WithLabelValues(provider).Set(state)
}

// IncMeteringEventsDropped implements Recorder.
func (m *Metrics) IncMeteringEventsDropped() { m.MeteringEventsDropped.Inc() }

// Compile-time proof *Metrics satisfies the Recorder port.
var _ Recorder = (*Metrics)(nil)

// NopRecorder is a Recorder that discards every signal. It lets the rate-limit
// middleware and breaker stay un-instrumented (e.g. in unit tests) without a nil
// check on every call site.
type NopRecorder struct{}

// IncRateLimitRejected implements Recorder (no-op).
func (NopRecorder) IncRateLimitRejected() {}

// SetBreakerState implements Recorder (no-op).
func (NopRecorder) SetBreakerState(string, float64) {}

// IncMeteringEventsDropped implements Recorder (no-op).
func (NopRecorder) IncMeteringEventsDropped() {}

var _ Recorder = NopRecorder{}
