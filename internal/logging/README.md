# internal/logging

Provides the structured `slog.Logger` (COMP-015) and the per-request HTTP
access-log middleware (FR-016). No global logger is used — the instance is
constructed once and threaded through the service via dependency injection
(ADR-0008).

## Key functions / types

| Symbol | Description |
|--------|-------------|
| `New(w, format, level) *slog.Logger` | Constructs a JSON or text `slog.Logger` writing to `w`. |
| `Middleware(logger) func(http.Handler) http.Handler` | Access-log middleware: emits one INFO record per completed request with `request_id`, `latency_ms`, `status_code`, `method`, and `path`. On panic: logs at ERROR with `panic_value` then re-panics so CARD-009 recovery middleware can respond. |
| `RequestIDFromContext(ctx) string` | Returns the request ID stored by `Middleware`, or `""`. |
| `LogPanic(ctx, logger, value, attrs...)` | Exported helper so the CARD-009 recovery middleware uses the same panic-logging contract. |

## statusRecorder

Internal `http.ResponseWriter` wrapper that captures the response status code
for the access log. Implements `http.Flusher` so streaming responses (SSE,
chunked) pass through correctly.

## Request IDs

`Middleware` reads `X-Request-ID` from the incoming request header; if absent,
it generates a random 128-bit hex ID. The ID is echoed back in the response
header and stored in `context.Context` for downstream use.
