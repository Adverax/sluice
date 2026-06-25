package metrics

import (
	"net/http"
	"strconv"
	"time"
)

// statusRecorder captures the response status code so the middleware can label
// http_requests_total{status}. It mirrors the recorder in internal/logging but
// is kept local so the two middlewares stay independent.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// Flush implements http.Flusher so streaming responses pass through.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware records the HTTP-level metrics for every request: it increments and
// decrements gateway_inflight_requests around the wrapped handler, times the
// request into http_request_duration_seconds{route}, and counts the outcome in
// http_requests_total{route,status}. The route label uses r.URL.Path; the
// status label is the integer status code observed on the response.
//
// It is intended to sit as an OUTER middleware (just inside logging/recovery) so
// the inflight gauge and latency cover the whole request, including downstream
// middleware. The inflight gauge is decremented in a defer so it is correct even
// if the handler panics (the recovery middleware translates that to a 500).
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.URL.Path
		start := time.Now()

		m.InflightRequests.Inc()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			m.InflightRequests.Dec()
			m.HTTPRequestDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
			m.HTTPRequestsTotal.WithLabelValues(route, strconv.Itoa(rec.status)).Inc()
		}()

		next.ServeHTTP(rec, r)
	})
}
