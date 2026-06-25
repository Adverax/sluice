# 02 — Circuit breaking (CAP-003)

A circuit breaker exists for one failure mode: when an upstream provider degrades,
every in-flight request keeps hammering it, ties up worker-pool slots and
goroutines waiting on slow calls, and the provider's pain cascades back into the
gateway. The breaker detects sustained failure and *fast-fails* — it stops
calling the bad provider for a cooldown, returning an immediate `503` instead of
a slow error, then probes for recovery. This is what lets `sluice` stay up and
recover under a provider outage (NFR-002).

`sluice` does not implement the state machine itself; it wraps
`github.com/sony/gobreaker` (`internal/breaker/breaker.go`, COMP-011) with
per-provider keying, tuned thresholds (ADR-0002), and two behaviours that gobreaker
does not give for free: **client-cancellation errors are not counted as provider
failures**, and the open state maps cleanly onto the gateway's `503` path.

See [diagrams/02-breaker-state-machine.puml](diagrams/02-breaker-state-machine.puml)
for the state machine.

---

## 1. The state machine and the trip thresholds

A gobreaker breaker has three states:

- **Closed** — calls pass through; outcomes are counted. The counter resets every
  `Interval` (a tumbling window).
- **Open** — calls are *not* made; `Execute` returns `ErrOpenState` immediately.
- **Half-open** — after `Timeout`, a limited number of probe calls (`MaxRequests`)
  are allowed through to test recovery.

The transitions are driven by the `Settings` `sluice` builds in
`defaultSettings`, which encodes the ADR-0002 *volume-based, 50%* policy:

```go
func (r *Registry) defaultSettings(name string) gobreaker.Settings {
	minReq := r.cfg.MinRequests
	ratio := r.cfg.FailureRatio
	return gobreaker.Settings{
		Name:        name,
		Interval:    r.cfg.Interval,
		Timeout:     r.cfg.Timeout,
		MaxRequests: r.cfg.MaxRequests,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests < minReq {
				return false
			}
			return float64(counts.TotalFailures)/float64(counts.Requests) >= ratio
		},
		OnStateChange: func(n string, from, to gobreaker.State) {
			if r.onStateChange != nil {
				r.onStateChange(n, from, to)
			}
		},
	}
}
```

`ReadyToTrip` is **volume-gated**: the breaker only trips once it has seen at
least `MinRequests` calls in the window *and* the failure ratio is at or above
`FailureRatio`. The volume gate prevents a single early failure (1/1 = 100%) from
tripping a healthy provider.

The concrete defaults come from `config.Breaker` (`internal/config/config.go`),
matching ADR-0002:

| Setting | Field | Default | Meaning |
|---------|-------|---------|---------|
| `Interval` | `Breaker.Interval` | `10s` | tumbling counter-reset period (closed) |
| `Timeout` | `Breaker.Timeout` | `60s` | open → half-open recovery period |
| `MaxRequests` | `Breaker.MaxRequests` | `5` | probe calls allowed in half-open |
| `MinRequests` | `Breaker.MinRequests` | `10` | min volume before the breaker may trip |
| `FailureRatio` | `Breaker.FailureRatio` | `0.5` | failure ratio at/above which it trips |
| (Retry-After hint) | `Breaker.RetryAfter` | `60s` | `Retry-After` on the fast-fail 503 |

So with defaults: in any 10s window, once a provider has handled ≥10 requests and
≥50% failed, the breaker opens; it stays open 60s, then admits up to 5 probes.

`WithSettings` lets tests inject a short `Timeout` so the open → half-open
transition is observable without a real 60s wait; production wiring leaves the
config defaults in place.

## 2. Per-provider keying

There is not one breaker but a registry of them, one per provider/model, built
lazily on first use (FR-007). A failing provider must not trip a healthy one:

```go
func (r *Registry) get(key string) *gobreaker.CircuitBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cb, ok := r.breakers[key]; ok {
		return cb
	}
	cb := gobreaker.NewCircuitBreaker(r.settingsFor(key))
	r.breakers[key] = cb
	return cb
}
```

The composition root keys the breaker by `req.Model` (in v1 one model maps to one
provider — see §4).

## 3. Open-state fast-fail, and what counts as a failure

When the breaker is open, `Execute` returns `ErrOpenState` immediately *without
invoking the underlying call* — latency < 1ms (INV-005). `ErrOpenState` is
re-exported so callers match the condition without importing gobreaker:

```go
var ErrOpenState = gobreaker.ErrOpenState
```

The subtle part is what gobreaker is allowed to *count*. A client hanging up
(`context.Canceled`) or timing out (`context.DeadlineExceeded`) is **not** the
provider's fault and must not trip a healthy provider's breaker. `Execute`
handles this by hiding ctx errors from gobreaker's counter — it returns them
inside the *result* (not the error position), so gobreaker records a success,
then unwraps them afterwards so the real error still reaches the caller:

```go
res, err := cb.Execute(func() (interface{}, error) {
	resp, callErr := call(ctx)
	if callErr != nil && (errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded)) {
		// Return the ctx error inside the result (not as the error return)
		// so gobreaker records this as a successful call and does NOT
		// increment its failure counter.
		return ctxErr{cause: callErr}, nil
	}
	return resp, callErr
})
...
// Unwrap a context-originated error that was hidden from gobreaker's counter.
if ce, ok := res.(ctxErr); ok {
	return provider.Response{}, ce.cause
}
```

Genuine provider errors (e.g. a 5xx `StatusError`) are returned in the error
position and *do* count toward the trip ratio.

### Streaming initiation

`ExecuteStream` applies the same breaker to stream *initiation* only. A
successful initiation counts as a breaker success; an initiation error counts as
a failure; the same per-provider registry is used. Mid-stream chunk errors
(arriving on the channel after a successful 200) are **not** fed back to the
breaker in v1 — a partial stream cannot be retried, and attributing a late
transport blip to the breaker would conflate two failure modes (documented in the
`ExecuteStream` doc comment). Client cancellation at initiation is excluded
exactly as in `Execute`.

## 4. Where the breaker sits — composition order (ADR-0006)

The breaker is not called directly; it is composed into the provider-call seam by
`internal/proxy/resilience/resilience.go` (the `Composer`) and wired in
`cmd/gateway/main.go`. There are two seams, with different orders:

**Unary** (`InferFunc`): `pool → retry → breaker → provider`.

```go
retrier := retry.New(cfg.Retry, retry.WithNonRetryable(resilience.IsOpenState))
breakers := breaker.NewRegistry(cfg.Breaker, ...)
composer := resilience.New(retrier, breakers, cfg.Breaker.RetryAfter)

workerPool := pool.New(cfg.WorkerPoolSize, cfg.Breaker.RetryAfter)
guardedInfer := workerPool.Guard(composer.InferFunc())
```

The crucial detail is `retry.WithNonRetryable(resilience.IsOpenState)`: the retry
engine treats `ErrOpenState` as non-retryable, so when the breaker is open the
retry layer does **not** spin against it — it fast-fails through without burning
the retry budget. Inside `InferFunc` the breaker is the innermost guard around the
single provider call:

```go
guarded := func(ctx context.Context) (provider.Response, error) {
	return c.breakers.Execute(ctx, key, func(ctx context.Context) (provider.Response, error) {
		return p.Infer(ctx, req)
	})
}
resp, err := c.retrier.Do(ctx, guarded)
```

**Streaming** (`StreamFunc`): `pool → breaker → provider`, with **no retry** — a
partially-sent stream cannot be safely replayed, so the retry engine is
deliberately absent from this seam:

```go
guardedStream := workerPool.GuardStream(composer.StreamFunc())
```

See [diagrams/03-composition-seam.puml](diagrams/03-composition-seam.puml).

## 5. Mapping the open state onto a 503

`InferFunc`/`StreamFunc` translate the open breaker (and a retry-deadline) into an
`*Unavailable` error carrying a reason and a `Retry-After` hint:

```go
if IsOpenState(err) {
	return provider.Response{}, &Unavailable{
		Reason:     "breaker_open",
		retryAfter: c.retryAfter,
		Err:        err,
	}
}
if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
	return provider.Response{}, &Unavailable{
		Reason:     "deadline",
		retryAfter: c.retryAfter,
		Err:        err,
	}
}
// Exhausted retries on a transient failure, or a non-retryable 4xx → 502.
return provider.Response{}, err
```

`*Unavailable` matches `server.ErrServiceUnavailable` (via its `Is` method) and
exposes `RetryAfter()`, so the server maps it to `503` + `Retry-After` through the
same path as pool saturation — without the server importing this package. On the
streaming path the open breaker is resolved *before* the `200`/SSE header is
written, so the client gets a real `503`, not a half-open stream (AC-014a).

The error-class summary, all from `resilience.go`:

| Cause | Result |
|-------|--------|
| breaker open (`ErrOpenState`) | `503` + `Retry-After` (`Reason: "breaker_open"`) |
| deadline / cancel during retry | `503` + `Retry-After` (`Reason: "deadline"`) |
| exhausted retries on transient 5xx | `502` (unary only) |
| non-retryable 4xx `StatusError` | `502` |

## 6. Observing state transitions

The registry forwards every transition to an injected `OnStateChange` callback
(it never imports Prometheus, ADR-0008). `cmd/gateway` uses it to both log the
transition and drive the `breaker_state` gauge:

```go
breaker.WithOnStateChange(func(name string, from, to gobreaker.State) {
	logger.Info("circuit breaker state change",
		slog.String("provider", name),
		slog.String("from", from.String()),
		slog.String("to", to.String()),
	)
	met.SetBreakerState(name, breakerStateValue(to))
})
```

`breakerStateValue` maps the gobreaker state onto the gauge values
`0=closed, 1=half-open, 2=open`. How that gauge is exported is covered by the
[Observability aspect](../../03-operations/observability/).

> Not determinable from code: the production values of `GATEWAY_BREAKER_*` are
> deployment configuration; the code fixes only the structural defaults in
> `config.Breaker` cited in §1.
