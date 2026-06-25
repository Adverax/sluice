package metrics_test

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/adverax/sluice/internal/metrics"
)

// newMetricsMux builds a ServeMux with one registered route, wrapped in the
// metrics middleware. Returns the handler and the *Metrics so the test can
// gather series directly.
func newMetricsMux(t *testing.T) (http.Handler, *metrics.Metrics) {
	t.Helper()
	reg := prometheus.NewRegistry()
	met := metrics.New(reg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := met.Middleware(mux)
	return handler, met
}

// gatherRouteLabels collects http_requests_total and returns the set of distinct
// "route" label values seen in the metric families.
func gatherRouteLabels(t *testing.T, met *metrics.Metrics) map[string]bool {
	t.Helper()
	mfs, err := met.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	routes := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != "http_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "route" {
					routes[lp.GetValue()] = true
				}
			}
		}
	}
	return routes
}

// TestMetrics_RouteCardinality_UnmatchedPath checks that a request to an unknown
// path is labelled route="other" rather than creating a per-path Prometheus
// series. This prevents unbounded cardinality from arbitrary/404 URLs.
func TestMetrics_RouteCardinality_UnmatchedPath(t *testing.T) {
	t.Parallel()

	handler, met := newMetricsMux(t)

	// Hit a path that is NOT registered on the mux → r.Pattern == "" after routing.
	req := httptest.NewRequest(http.MethodGet, "/totally/unknown/path/99999", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	routes := gatherRouteLabels(t, met)

	if routes["/totally/unknown/path/99999"] {
		t.Error("http_requests_total has a series with raw path label; unbounded cardinality detected")
	}
	if !routes["other"] {
		t.Error("http_requests_total is missing route=\"other\" for unmatched path")
	}
}

// TestMetrics_RouteCardinality_MatchedRoute checks that a request to a registered
// route uses the route TEMPLATE as the "route" label value, not the raw path with
// parameter values substituted in.
func TestMetrics_RouteCardinality_MatchedRoute(t *testing.T) {
	t.Parallel()

	handler, met := newMetricsMux(t)

	// Hit the registered pattern with a concrete path-parameter value.
	req := httptest.NewRequest(http.MethodGet, "/v1/items/abc-123", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	routes := gatherRouteLabels(t, met)

	if routes["abc-123"] || routes["/v1/items/abc-123"] {
		t.Error("http_requests_total has a raw path label; want route template label")
	}
	// The pattern registered is "GET /v1/items/{id}".
	found := false
	for r := range routes {
		if strings.Contains(r, "/v1/items/") {
			found = true
		}
	}
	if !found {
		t.Errorf("http_requests_total routes %v does not contain the route template /v1/items/…", routes)
	}
}

// TestMetrics_Unwrap_FlushReachesBase verifies that http.NewResponseController
// can Flush through the statusRecorder wrapper via Unwrap().
func TestMetrics_Unwrap_FlushReachesBase(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	met := metrics.New(reg)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /flush", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.Flush(); err != nil {
			// httptest.ResponseRecorder supports Flush, so this must not error.
			t.Errorf("Flush through statusRecorder wrapper failed: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := met.Middleware(mux)
	req := httptest.NewRequest(http.MethodPost, "/flush", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
}

// fakeMetricsHijackWriter is an http.ResponseWriter that also implements
// http.Hijacker so we can verify Unwrap() reaches it through the statusRecorder.
type fakeMetricsHijackWriter struct {
	httptest.ResponseRecorder
}

func (f *fakeMetricsHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// TestMetrics_Unwrap_HijackReachesBase verifies that Hijack is forwarded through
// the statusRecorder wrapper (not silently swallowed), so SSE / WebSocket
// handlers added by CARD-004 can upgrade connections even when metrics middleware
// is present.
func TestMetrics_Unwrap_HijackReachesBase(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	met := metrics.New(reg)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /hijack", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		_, _, err := rc.Hijack()
		if err == nil {
			t.Error("expected Hijack error from fakeMetricsHijackWriter, got nil")
		}
		if strings.Contains(err.Error(), "not implemented by handler") {
			t.Errorf("Hijack was NOT forwarded through statusRecorder (got %q); Unwrap() may be missing", err.Error())
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := met.Middleware(mux)
	fake := &fakeMetricsHijackWriter{ResponseRecorder: *httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/hijack", nil)
	handler.ServeHTTP(fake, req)
}
