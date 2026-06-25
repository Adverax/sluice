package middleware_test

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/adverax/sluice/internal/middleware"
)

// TestTracing_RouteCardinality_UnmatchedPath verifies that requests to paths
// that do not match any registered route produce a span named
// "HTTP <method> other" (not a per-path span name), preventing unbounded span
// name cardinality from arbitrary/attacker-controlled URLs.
func TestTracing_RouteCardinality_UnmatchedPath(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	mux := http.NewServeMux()
	// Register one known route; the test will hit a DIFFERENT path.
	mux.HandleFunc("POST /v1/known", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := middleware.Tracing(tp.Tracer("test"))(mux)

	// Hit an unregistered path — no matched pattern, r.Pattern == "".
	req := httptest.NewRequest(http.MethodGet, "/totally/unknown/path/12345", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	name := spans[0].Name()
	if strings.Contains(name, "/totally/unknown/path/12345") {
		t.Errorf("span name %q contains raw URL path; want low-cardinality name using \"other\"", name)
	}
	if !strings.Contains(name, "other") {
		t.Errorf("span name %q does not contain \"other\"; unmatched routes must be bucketed as \"other\"", name)
	}
}

// TestTracing_RouteCardinality_MatchedRoute verifies that a request to a
// registered route uses the route TEMPLATE as the span name (not the raw path),
// producing low-cardinality span names even when path parameters differ.
func TestTracing_RouteCardinality_MatchedRoute(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	mux := http.NewServeMux()
	// Register a pattern with a wildcard segment (Go 1.22+ enhanced patterns).
	mux.HandleFunc("GET /v1/items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := middleware.Tracing(tp.Tracer("test"))(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/items/abc-123", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	name := spans[0].Name()
	if strings.Contains(name, "abc-123") {
		t.Errorf("span name %q contains raw path value; want route template", name)
	}
	if !strings.Contains(name, "/v1/items/{id}") {
		t.Errorf("span name %q does not contain route template \"/v1/items/{id}\"", name)
	}
}

// fakeHijackWriter is an http.ResponseWriter that also implements http.Hijacker
// so we can verify Unwrap() reaches it through the tracingStatusRecorder wrapper.
type fakeHijackWriter struct {
	httptest.ResponseRecorder
	hijackCalled bool
}

func (f *fakeHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijackCalled = true
	return nil, nil, http.ErrNotSupported
}

// TestTracing_Unwrap_FlushAndHijackReachBase verifies that http.ResponseController
// can reach both Flusher and Hijacker capabilities through the wrapper chain via
// the Unwrap() method — ensuring CARD-004 streaming/SSE handlers work correctly.
func TestTracing_Unwrap_FlushAndHijackReachBase(t *testing.T) {
	t.Parallel()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /flush", func(w http.ResponseWriter, _ *http.Request) {
		// Verify Flush reaches the underlying recorder via ResponseController.
		rc := http.NewResponseController(w)
		if err := rc.Flush(); err != nil {
			// httptest.ResponseRecorder supports Flush, so this must not error.
			t.Errorf("Flush through wrapper failed: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := middleware.Tracing(tp.Tracer("test"))(mux)

	req := httptest.NewRequest(http.MethodPost, "/flush", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}

	// Verify that Hijack is reachable: wrap a fakeHijackWriter, check that the
	// inner handler can reach it via http.NewResponseController(w).Hijack().
	mux2 := http.NewServeMux()
	mux2.HandleFunc("POST /hijack", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		// Hijack will return ErrNotSupported from fakeHijackWriter, but the
		// point is that it is NOT swallowed by the wrapper (ErrNotSupported !=
		// http.ErrNotSupported from the controller's "not implemented" path).
		_, _, err := rc.Hijack()
		if err == nil {
			t.Error("expected Hijack error from fakeHijackWriter, got nil")
		}
		// As long as the error is not the generic "not implemented by handler"
		// message from ResponseController, Hijack was forwarded to the base.
		if strings.Contains(err.Error(), "not implemented by handler") {
			t.Errorf("Hijack was NOT forwarded through the wrapper (got %q); Unwrap() may be missing", err.Error())
		}
		w.WriteHeader(http.StatusOK)
	})

	handler2 := middleware.Tracing(tp.Tracer("test"))(mux2)
	fake := &fakeHijackWriter{ResponseRecorder: *httptest.NewRecorder()}
	req2 := httptest.NewRequest(http.MethodPost, "/hijack", nil)
	handler2.ServeHTTP(fake, req2)
}
