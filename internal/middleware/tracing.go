package middleware

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Tracing is the OTel HTTP middleware (COMP-014, FR-011). It starts a root span
// for each incoming request and stores the span context on the request context
// so the nested provider-call span (started by the instrumented InferFunc)
// becomes its child — producing the >=2-span trace AC-030 requires. The span is
// ended in a defer regardless of outcome, and the response status is recorded as
// a span attribute.
//
// Cardinality: the span is started with a low-cardinality name ("HTTP <method>")
// to avoid unbounded span names from raw URL paths. After next.ServeHTTP returns
// the span name and http.route attribute are updated to the matched ServeMux
// pattern (r.Pattern, set by Go 1.22+ net/http). Unmatched paths (404s) are
// bucketed under "other". The raw path may be added as http.target (an
// acceptable high-cardinality span attribute per OTel semconv) but must never
// appear in the span NAME or a metric label.
//
// Export of these spans is asynchronous (batch processor, see internal/tracing),
// so a down collector never blocks the request (AC-050); this middleware only
// creates spans, it never waits on export.
func Tracing(tracer trace.Tracer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Start the span with a low-cardinality name; the matched route
			// template is not yet known before the mux resolves the handler.
			ctx, span := tracer.Start(r.Context(), "HTTP "+r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.target", r.URL.RequestURI()),
				),
			)
			defer span.End()

			rec := &tracingStatusRecorder{ResponseWriter: w, status: http.StatusOK}
			// Keep a reference to the context-enriched copy so we can read
			// r2.Pattern after the mux sets it during ServeHTTP. r.WithContext
			// makes a shallow copy; the mux mutates r2.Pattern on that copy,
			// not on the original r.
			r2 := r.WithContext(ctx)
			next.ServeHTTP(rec, r2)

			// After routing, r2.Pattern holds the matched route template (Go 1.22+).
			// Use it as the canonical low-cardinality span name and http.route
			// attribute. Unmatched requests (404) use "other".
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

// tracingStatusRecorder captures the status code for the span attribute.
//
// Unwrap exposes the underlying ResponseWriter so http.ResponseController (and
// net/http's capability detection) can reach the base writer — this preserves
// Flusher AND Hijacker for streaming/SSE handlers without hand-forwarding each
// interface individually.
type tracingStatusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *tracingStatusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *tracingStatusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func (r *tracingStatusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter so http.ResponseController and
// net/http's interface-capability detection (Flusher, Hijacker, etc.) can reach
// the base writer through this wrapper.
func (r *tracingStatusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
