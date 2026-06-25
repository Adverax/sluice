package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// ctxKey is the unexported type for context values set by this package.
type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDFromContext returns the request id stored by Middleware, or "" if
// none is present.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// statusRecorder captures the response status code so the middleware can log it.
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
		// Implicit 200 on first write without an explicit WriteHeader.
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// Flush implements http.Flusher so streaming responses (SSE, chunked) pass
// through to the underlying ResponseWriter. Without this, wrapping the
// ResponseWriter in statusRecorder would silently drop Flush calls.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware returns an HTTP middleware that, on every completed request, emits
// an slog record at INFO level carrying request_id, latency_ms and status_code
// (FR-016 / AC-040). If the wrapped handler panics, the panic is logged at
// ERROR level with a panic_value field (FR-016 / AC-041) and then re-raised so
// the dedicated recovery middleware (CARD-009) can translate it into a 500.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" {
				requestID = newRequestID()
			}
			ctx := context.WithValue(r.Context(), requestIDKey, requestID)
			r = r.WithContext(ctx)
			w.Header().Set("X-Request-ID", requestID)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			defer func() {
				latencyMS := time.Since(start).Milliseconds()
				if rv := recover(); rv != nil {
					LogPanic(ctx, logger, rv,
						slog.String("request_id", requestID),
						slog.Int64("latency_ms", latencyMS),
					)
					// Re-panic so CARD-009 recovery middleware (or the server)
					// handles the HTTP response. The logging side is this card.
					panic(rv)
				}
				logger.LogAttrs(ctx, slog.LevelInfo, "request completed",
					slog.String("request_id", requestID),
					slog.Int64("latency_ms", latencyMS),
					slog.Int("status_code", rec.status),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

// LogPanic records a recovered panic at ERROR level with a panic_value field
// (AC-041). It is exported so the recovery middleware in CARD-009 can reuse the
// exact same logging contract. Additional attrs (e.g. request_id) are appended.
func LogPanic(ctx context.Context, logger *slog.Logger, panicValue any, attrs ...slog.Attr) {
	all := append([]slog.Attr{slog.Any("panic_value", panicValue)}, attrs...)
	logger.LogAttrs(ctx, slog.LevelError, "panic recovered", all...)
}

// newRequestID returns a random 128-bit hex identifier. On the (practically
// impossible) failure of the system RNG it falls back to a timestamp so a
// request is never left without an id.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}
