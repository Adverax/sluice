package middleware

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Tracing is the OTel HTTP middleware (COMP-014, FR-011). It starts a root span
// for each incoming request (named "HTTP <method> <path>") and stores the
// span context on the request context so the nested provider-call span (started
// by the instrumented InferFunc) becomes its child — producing the >=2-span
// trace AC-030 requires. The span is ended in a defer regardless of outcome,
// and the response status is recorded as a span attribute.
//
// Export of these spans is asynchronous (batch processor, see internal/tracing),
// so a down collector never blocks the request (AC-050); this middleware only
// creates spans, it never waits on export.
func Tracing(tracer trace.Tracer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, span := tracer.Start(r.Context(), "HTTP "+r.Method+" "+r.URL.Path,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.route", r.URL.Path),
				),
			)
			defer span.End()

			rec := &tracingStatusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			span.SetAttributes(attribute.Int("http.status_code", rec.status))
			if rec.status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}

// tracingStatusRecorder captures the status code for the span attribute.
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
