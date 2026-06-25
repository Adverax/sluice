# internal/pool

Bounded worker pool / backpressure (COMP-010) — caps concurrent **upstream** provider
calls and sheds excess load immediately (reject-before-work) instead of queueing or
spawning unbounded goroutines.

## How it works

A buffered-channel semaphore of capacity `GATEWAY_WORKER_POOL_SIZE` (default 100,
ADR-0003). `Guard` wraps a `server.InferFunc`:

- **Acquire** is non-blocking (`select { case sem<-{}: …; default: … }`). When full it
  returns `ErrPoolSaturated` **immediately** — no goroutine spawned, no blocking (INV-001).
- The slot is released via `defer` on **every** return path (success, error, panic), so
  freed slots accept work instantly and concurrency never exceeds the cap (NFR-006).
- The saturated error `Is`-matches `server.ErrServiceUnavailable` and carries `RetryAfter()`,
  so the server's existing path maps it to **503 + Retry-After** with no new code.

## Composition (ADR-0006)

Wired in `cmd/gateway` as the **outermost** layer of the upstream-call seam:

```
pool.Guard( resilience.InferFunc )   →   pool → retry → breaker → provider
```

A saturated reject therefore does **not** consume a retry attempt or a breaker slot.
The `server.InferFunc` signature is unchanged, so CARD-005's rate-limit middleware sits
**outside** the pool (an earlier/outer layer).

## API

- `New(size, retryAfter) *Pool` — panics on non-positive size (fail-fast at boot).
- `(*Pool).Guard(next server.InferFunc) server.InferFunc` · package-level `Guard(size, retryAfter, next)`.
- `ErrPoolSaturated` — typed sentinel; `InFlight()` — current held slots (testing/observability).

> Shed-load events are not yet metered here — a counter lands with observability/metering
> (CARD-009/010). Full-scale overload soak (3×/500 RPS, 2 min, pprof baseline) is CARD-011's
> load harness; `bench_test.go` holds small in-package proxies for the same invariants.
