# internal/lifecycle

Implements COMP-006, the Lifecycle Manager. It owns the inbound `*http.Server`,
starts it, and performs a graceful shutdown on SIGINT/SIGTERM — draining
in-flight application requests before the process exits (FR-012 / NFR-005).
The logger and server are injected; no globals (ADR-0008).

## Key types

| Type | Description |
|------|-------------|
| `Manager` | Wraps an `*http.Server` with graceful-drain logic. Construct with `New`. |

## Key functions

| Function | Description |
|----------|-------------|
| `New(server, logger, shutdownTimeout) *Manager` | Constructor. `shutdownTimeout <= 0` falls back to 30 s. |
| `Manager.Run(ctx) error` | Starts the server in a goroutine and blocks until `ctx` is cancelled or the server fails. On cancellation, calls `Shutdown` with a `shutdownTimeout` deadline. Returns `nil` on a clean drain. |
| `Manager.CountingMiddleware(next) http.Handler` | Middleware that atomically increments/decrements an in-flight counter. Mount it only on application routes — **not** on `/healthz` or `/readyz`. |
| `Manager.InFlight() int64` | Returns the current in-flight request count (test / metrics use). |

## Graceful drain

When `ctx` is cancelled (i.e. SIGINT/SIGTERM received):

1. The current in-flight count is sampled and logged.
2. `http.Server.Shutdown` is called with a `shutdownTimeout`-bounded context.
3. On success, `"drained N requests"` is logged and `Run` returns `nil`.
4. On timeout, the error is logged and returned (exit code 1).

## Gateway wiring

`cmd/gateway` mounts `/healthz` and `/readyz` directly on the outer mux (not
through `CountingMiddleware`) so probe traffic does not appear in the drain log.
