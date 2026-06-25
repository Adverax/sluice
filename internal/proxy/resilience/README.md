# internal/proxy/resilience

## Purpose

Composition root (ADR-0006) that wires the retry engine (COMP-003,
`internal/proxy/retry`) and the per-provider circuit breaker (COMP-011,
`internal/breaker`) into the single `server.InferFunc` seam the server
exposes. This is the only package that knows about both layers; neither
retry nor breaker imports the other.

## Architecture

**Composition order** (ADR-0006):

```
server.InferFunc
    └─ retry.Engine.Do(ctx, guarded)
           └─ guarded = breaker.Registry.Execute(ctx, key, p.Infer(ctx, req))
```

`key` is `req.Model` — one breaker per model/provider (FR-007, v1: one model
maps to one provider).

**Error mapping at the server boundary:**

| Condition | Error produced | Server maps to |
|-----------|---------------|----------------|
| Open breaker (`ErrOpenState`) | `*Unavailable{Reason:"breaker_open"}` | 503 + `Retry-After` (AC-022) |
| Deadline/cancellation during retry | `*Unavailable{Reason:"deadline"}` | 503 + `Retry-After` (AC-020) |
| Retries exhausted on 5xx | `retry.ErrExhausted` (propagated) | 502 (AC-019) |
| Non-retryable 4xx | `*provider.StatusError` (propagated unwrapped) | 502 (AC-021) |

`*Unavailable` satisfies `errors.Is(err, server.ErrServiceUnavailable)` via its
`Is` method — the server classifies it without importing this package (no import
cycle). It also implements `RetryAfter() time.Duration` so the server can set the
`Retry-After` header.

The seam is intentionally clean for CARD-008: `InferFunc` returns the same
signature, so the worker pool wraps the composed func without changes.

## Key types

| Type | Description |
|------|-------------|
| `Composer` | Holds the retry engine and breaker registry. Construct with `New`. |
| `Unavailable` | Error returned on fast-fail (open breaker or deadline); implements `errors.Is` against `server.ErrServiceUnavailable` and exposes `RetryAfter()`. |
| `IsOpenState` | Predicate passed to `retry.WithNonRetryable` so the retry loop never spins against an open breaker. |

## Usage

```go
retrier := retry.New(cfg.Retry, retry.WithNonRetryable(resilience.IsOpenState))
breakers := breaker.NewRegistry(cfg.Breaker)
composer := resilience.New(retrier, breakers, cfg.Breaker.RetryAfter)

srv := server.New(router, healthHandler, logger,
    server.WithInferFunc(composer.InferFunc()),
)
```

## See also

- `internal/proxy/retry` — retry engine (COMP-003)
- `internal/breaker` — circuit breaker registry (COMP-011)
- `internal/server` — `InferFunc` seam and `ErrServiceUnavailable` sentinel
- ADR-0006: `meta/architecture/decisions/adr/0006-proxy-resilience-integration-hybrid.md`
