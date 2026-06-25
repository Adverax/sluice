# CAP-005 — Runtime lifecycle

> Source of truth: `internal/lifecycle/lifecycle.go`,
> `internal/middleware/recover.go`, `cmd/gateway/main.go`,
> `internal/metering/worker.go` (the hook it flushes).

## 1. Why this lives in the Proxy context

CAP-005 covers the two ways the process is asked to stop serving cleanly:
**graceful shutdown** on a signal, and **panic recovery** inside a handler. Both
operate directly on in-flight request state — draining requests, recovering the
request goroutine — which is why the domain model keeps them in the Proxy context
(CTX-001) alongside the hot path rather than in Observability or Resilience. The
guarantees are FR-012 (drain in-flight + final metering flush on SIGTERM), NFR-005
(zero in-flight requests dropped), and FR-013 (panic → 500, process survives).

## 2. Graceful shutdown — the trigger

The composition root arms a signal-cancelled context and blocks on the lifecycle
`Manager`:

```go
// cmd/gateway/main.go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()
return manager.Run(ctx)
```

`Manager.Run` (`internal/lifecycle/lifecycle.go`) starts `ListenAndServe` in a
goroutine and selects: if serving fails it returns the error; if `ctx` is
cancelled (a signal arrived) it transitions into `shutdown()`:

```go
select {
case err := <-serveErr:
    if err != nil { return fmt.Errorf("server failed: %w", err) }
    return nil
case <-ctx.Done():
    return m.shutdown()
}
```

See [diagrams/02-runtime-lifecycle-01.puml](diagrams/02-runtime-lifecycle-01.puml).

## 3. Counting in-flight requests

The drain count is tracked by `CountingMiddleware`, which sits in the chain (just
outside the cache — Section 2 of `01-inference-proxying.md`) so application
requests are counted but the probe endpoints are not:

```go
func (m *Manager) CountingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        atomic.AddInt64(&m.inFlight, 1)
        defer atomic.AddInt64(&m.inFlight, -1)
        next.ServeHTTP(w, r)
    })
}
```

## 4. The drain (FR-012 / NFR-005)

`shutdown()` samples the in-flight count, logs that it is draining, then calls
`http.Server.Shutdown` bounded by `shutdownTimeout` (config
`cfg.Server.ShutdownTimeout`; falls back to 30 s if unset). `Shutdown` stops
accepting new connections and waits for active handlers to finish:

```go
draining := atomic.LoadInt64(&m.inFlight)
m.logger.LogAttrs(..., "shutdown signal received, draining", slog.Int64("in_flight", draining))

shutdownCtx, cancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
defer cancel()

drained := draining
if err := m.server.Shutdown(shutdownCtx); err != nil {
    if errors.Is(err, context.DeadlineExceeded) {
        unfinished := atomic.LoadInt64(&m.inFlight)
        drained = draining - unfinished
        m.logger.LogAttrs(..., fmt.Sprintf("forced shutdown: %d requests unfinished", unfinished), ...)  // WARN
    } else {
        drained = 0
        ... // "graceful shutdown failed" at ERROR; shutdownErr set
    }
} else {
    m.logger.LogAttrs(..., fmt.Sprintf("drained %d requests", draining), ...)  // INFO
}
```

Three outcomes, all derived from the real branches:

- **Clean drain:** every in-flight handler finished within the timeout → INFO
  `drained N requests`.
- **Forced shutdown (AC-051):** the timeout elapsed; `Shutdown` returns
  `context.DeadlineExceeded`. This is *not* a hard failure — the process must
  still exit cleanly. It logs a WARN `forced shutdown: N requests unfinished` and
  proceeds to the hooks.
- **Other error:** logged at ERROR and surfaced as the returned error.

## 5. Post-drain hooks with their own deadline

After the HTTP drain, registered `OnShutdown` hooks run **in registration order**,
each with its **own fresh-deadline context** derived from `context.Background()` —
deliberately *not* the (possibly exhausted) HTTP-drain context — so the metering
flush always gets a real budget even on the forced path:

```go
for _, hook := range m.onShutdown {
    hookCtx, hookCancel := context.WithTimeout(context.Background(), m.hookTimeout)
    err := hook(hookCtx)
    hookCancel()
    if err != nil { ... if shutdownErr == nil { shutdownErr = fmt.Errorf("shutdown hook: %w", err) } }
}
```

`hookTimeout` defaults to 5 s and is set via `WithHookTimeout(cfg.Shutdown.HookTimeout)`
in `main.go`. The hooks wired there, in order, are:

```go
manager.OnShutdown(meteringWorker.Close)        // flush remaining buffered usage events
if stopMockUpstream != nil { manager.OnShutdown(stopMockUpstream) }  // stop in-process mock upstream
```

The metering worker's `Close(ctx)` (`internal/metering/worker.go`) signals its
stop channel and waits for the final batch flush, bounded by the hook's ctx. It
runs **after** the HTTP drain so no new events are being enqueued by the time it
drains the buffer. The full metering flush mechanism is documented in
[Metering](../../04-integrations/metering/).

## 6. The drained / flushed log line (AC-015c)

After the hooks have run, the manager reads the flushed count and emits a single
summary line. The count is read here, *post-hooks*, because the worker only knows
it after `Close`:

```go
if m.flushedCountFn != nil {
    flushed := m.flushedCountFn()
    m.logger.LogAttrs(..., fmt.Sprintf("shutdown complete: drained %d requests, flushed %d usage events", drained, flushed),
        slog.Int64("drained", drained), slog.Int("flushed_usage_events", flushed))
}
```

`flushedCountFn` is wired to `meteringWorker.FlushedOnShutdown` via
`WithFlushedCountFn` in `main.go`; that method returns the atomically-tracked
count of events persisted during the drain. `Run` returns `shutdownErr` (nil on a
clean drain), which `main` maps to exit code 0.

## 7. Panic recovery middleware (FR-013)

`Recoverer` (`internal/middleware/recover.go`) is the **outermost** middleware in
the chain (see `01-inference-proxying.md` §2). A deferred `recover()` catches any
panic from a downstream handler, logs it at ERROR with the panic value and a full
stack trace, and writes a 500 — the process keeps serving:

```go
defer func() {
    if rv := recover(); rv != nil {
        if rv == http.ErrAbortHandler {
            panic(rv)   // not a real panic — let net/http abort the connection as intended
        }
        logging.LogPanic(r.Context(), logger, rv,
            slog.String("request_id", logging.RequestIDFromContext(r.Context())),
            slog.String("stack", string(debug.Stack())),
        )
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusInternalServerError)
        _, _ = w.Write([]byte(`{"error":"internal_error","message":"internal server error"}`))
    }
}()
next.ServeHTTP(w, r)
```

Two behaviours are load-bearing (see
[diagrams/02-runtime-lifecycle-02.puml](diagrams/02-runtime-lifecycle-02.puml)):

- **`http.ErrAbortHandler` passthrough.** This sentinel is how net/http
  deliberately aborts a connection (e.g. on a flushed/hijacked response — exactly
  the streaming path). It is re-raised, not masked as a 500, so net/http handles
  the abort as intended.
- **Best-effort 500.** If the handler already wrote headers, `WriteHeader` is a
  no-op (net/http logs a superfluous-call warning), but the process still
  survives — the AC-033 guarantee.

`Recoverer` is layered *outside* the logging middleware, which logs the panic at
ERROR and re-panics so the request-completed line is suppressed; this outer
recover performs the final recover and stops the re-panic from unwinding into
net/http's own per-connection recovery (which would abort without a clean 500
body).

## 8. SafeGo — panics in detached goroutines (AC-034)

`recover()` only intercepts panics on its **own** goroutine's stack, and an
unrecovered panic in *any* goroutine crashes the whole process. So any goroutine
a handler detaches must be launched with `SafeGo`, which installs its own
recover:

```go
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
```

The Recoverer middleware guards only the request goroutine; `SafeGo` is the
mechanism that keeps a background goroutine's panic from taking the process down.

## 9. Not determinable from code

- The shutdown sequence is fully observable from `lifecycle.go` and the `main.go`
  wiring. Whether a *specific* request actually drains in time (NFR-005's "0
  dropped") depends on `shutdownTimeout` versus real handler latency and is a
  test/benchmark property (`TestGracefulShutdown_ZeroDropped`,
  `TestGracefulShutdown_TimeoutForced`), not derivable from the code paths alone.
