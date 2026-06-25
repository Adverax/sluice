package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/adverax/sluice/internal/logging"
)

// Recoverer is the panic-recovery middleware (COMP-007, FR-013). A deferred
// recover() catches any panic raised by a downstream handler; it logs the panic
// at ERROR with the panic value and a full stack trace (reusing
// logging.LogPanic so the log contract — the panic_value field at ERROR — is
// identical to the one already covered by TestLogging_PanicLoggedAtError,
// AC-041), then writes a 500 to the client (AC-033). The process is NOT torn
// down: control returns to net/http, which keeps serving subsequent requests.
//
// Placement (ADR-0006 composition order): Recoverer is installed as the
// OUTERMOST middleware, wrapping the logging middleware. The logging middleware
// logs the panic at ERROR and RE-panics (see internal/logging/middleware.go) so
// that the request-completed line is suppressed for a panicked request; this
// outer Recoverer then performs the final recover, emits the 500, and stops the
// re-panic from unwinding into net/http's own per-connection recovery (which
// would abort the connection without a clean 500 body).
//
// A panic in a DETACHED goroutine spawned by a handler cannot be caught here:
// Go's recover only intercepts panics on the SAME goroutine's stack, and an
// unrecovered panic in any goroutine crashes the whole process (AC-034). Such
// goroutines MUST therefore be launched with SafeGo, which installs its own
// recover. This middleware only guards the request goroutine.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					// http.ErrAbortHandler is the sentinel net/http uses to abort a
					// connection deliberately (e.g. a flushed/hijacked response). It is
					// not a real panic; re-raise it so net/http handles it as intended
					// rather than masking it with a 500.
					if rv == http.ErrAbortHandler {
						panic(rv)
					}

					logging.LogPanic(r.Context(), logger, rv,
						slog.String("request_id", logging.RequestIDFromContext(r.Context())),
						slog.String("stack", string(debug.Stack())),
					)

					// Best-effort 500. If the handler already wrote headers, WriteHeader
					// is a no-op (net/http logs a superfluous-call warning), but the
					// process still survives — which is the AC-033 guarantee.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":"internal_error","message":"internal server error"}`))
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// SafeGo launches fn in a new goroutine guarded by a deferred recover so a panic
// inside fn cannot crash the process (AC-034). An unrecovered panic in ANY
// goroutine terminates the whole program, and a request handler's defer can only
// recover panics on its OWN stack — so any goroutine a handler detaches must be
// started through SafeGo rather than a bare `go fn()`. On a recovered panic the
// value and stack are logged at ERROR via logging.LogPanic, identical to the
// request-path contract.
func SafeGo(logger *slog.Logger, fn func()) {
	go func() {
		defer func() {
			if rv := recover(); rv != nil {
				logging.LogPanic(context.Background(), logger, rv,
					slog.String("origin", "safego"),
					slog.String("stack", string(debug.Stack())),
				)
			}
		}()
		fn()
	}()
}
