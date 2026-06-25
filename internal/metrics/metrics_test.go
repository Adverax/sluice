package metrics_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/metrics"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/server"
)

// requiredMetricNames is the full NFR-007/AC-048 set of six metrics that GET
// /metrics must expose.
var requiredMetricNames = []string{
	"http_requests_total",
	"http_request_duration_seconds",
	"gateway_inflight_requests",
	"provider_request_duration_seconds",
	"ratelimit_rejected_total",
	"breaker_state",
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newGateway builds the generated HTTP boundary wrapped in the metrics
// middleware and instrumented InferFunc, with a freshly injected registry
// (ADR-0008). It returns the handler and the metrics handle so a test can drive
// requests and then scrape /metrics off the SAME registry.
func newGateway(t *testing.T) (http.Handler, *metrics.Metrics) {
	t.Helper()

	reg := prometheus.NewRegistry()
	met := metrics.New(reg)

	router := proxy.NewRouter()
	router.Register("mock", provider.New(provider.WithResponse(provider.Response{
		Model:        "mock",
		Content:      "ok",
		FinishReason: "stop",
	})))

	// Default InferFunc (provider.Infer) instrumented with provider duration so
	// provider_request_duration_seconds is populated by a successful call.
	defaultInfer := func(ctx context.Context, p provider.Provider, req provider.Request) (provider.Response, error) {
		return p.Infer(ctx, req)
	}

	hh := health.New(discardLogger(), 0)
	srv := server.New(router, hh, discardLogger(),
		server.WithInferFunc(met.InstrumentInferFunc(defaultInfer)),
		server.WithMetricsRegistry(reg),
	)
	// metrics middleware wraps the generated routes so http_* are recorded.
	handler := met.Middleware(srv.Handler(http.NewServeMux()))
	return handler, met
}

func scrapeMetrics(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: got status %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func driveRequests(t *testing.T, h http.Handler, met *metrics.Metrics) {
	t.Helper()
	body := `{"model":"mock","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/chat/completions: got status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
	}
	// Touch the recorder-driven metrics (ratelimit + breaker) so their series are
	// registered/exported even without a real 429/trip in this unit test.
	met.IncRateLimitRejected()
	met.SetBreakerState("mock", metrics.BreakerStateClosed)
}

// TestMetrics_ExposesRequiredMetrics covers AC-029: after several requests, GET
// /metrics returns the FR-010 metric names in Prometheus text format.
func TestMetrics_ExposesRequiredMetrics(t *testing.T) {
	t.Parallel()

	h, met := newGateway(t)
	driveRequests(t, h, met)

	out := scrapeMetrics(t, h)

	// AC-029 names (the five called out in the AC). provider_request_duration is
	// additionally asserted by TestMetrics_AllSixMetricsPresent.
	for _, name := range []string{
		"http_requests_total",
		"http_request_duration_seconds",
		"gateway_inflight_requests",
		"ratelimit_rejected_total",
		"breaker_state",
	} {
		if !strings.Contains(out, name) {
			t.Errorf("GET /metrics output missing metric %q", name)
		}
	}
}

// TestMetrics_AllSixMetricsPresent covers AC-048 (NFR-007): all six required
// metrics are present in the exposition.
func TestMetrics_AllSixMetricsPresent(t *testing.T) {
	t.Parallel()

	h, met := newGateway(t)
	driveRequests(t, h, met)

	out := scrapeMetrics(t, h)

	for _, name := range requiredMetricNames {
		if !strings.Contains(out, name) {
			t.Errorf("GET /metrics output missing required metric %q", name)
		}
	}
}
