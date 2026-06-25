# internal/breaker

## Purpose

Implements COMP-011 — the per-provider circuit breaker (FR-007), on top of
`github.com/sony/gobreaker` tuned per ADR-0002 (`volume_based_50pct`). Isolates
upstream provider failures so that a degraded provider does not exhaust gateway
goroutines: once a provider's error rate crosses the threshold the breaker opens
and all subsequent calls fast-fail with `ErrOpenState` in < 1 ms (INV-005,
AC-022).

## Architecture

The registry holds one `gobreaker.CircuitBreaker` per provider/model key, created
lazily on first use. It exposes the same `Call` shape as `internal/proxy/retry`,
so the two compose directly (ADR-0006):

```
retry.Engine.Do(ctx, breaker.Registry.Execute(ctx, key, providerCall))
```

**ADR-0002 default settings (volume_based_50pct):**

| Parameter | Value | Notes |
|-----------|-------|-------|
| `Interval` | 10s | Tumbling counter-reset period (not a sliding window) |
| `Timeout` | 60s | Open → half-open recovery |
| `MaxRequests` | 5 | Probe requests allowed in half-open |
| `MinRequests` | 10 | Minimum volume before tripping |
| `FailureRatio` | 0.5 | Failure ratio threshold |

**Context cancellation is NOT counted as a failure.** A client hanging up or
timing out is the client's fault, not the provider's. The `Execute` method wraps
such errors in a non-error result so `gobreaker` records the call as successful,
then unwraps the original context error after `Execute` returns.

## Key types

| Type | Description |
|------|-------------|
| `Registry` | Per-provider breaker registry; safe for concurrent use. Construct with `NewRegistry`. |
| `Call` | `func(ctx context.Context) (provider.Response, error)` — identical to `retry.Call`. |
| `ErrOpenState` | Re-export of `gobreaker.ErrOpenState`; callers match on this without importing gobreaker directly. |
| `Option` | Functional option (`WithOnStateChange`, `WithSettings`). |

`WithSettings` overrides how `gobreaker.Settings` are built per key — used in
tests to inject a short `Timeout` so half-open transitions are observable without
waiting 60s (AC-024).

## Environment variables

Populated via `internal/config.Breaker` from `config.Load()`.

| Variable | Default | Description |
|----------|---------|-------------|
| `GATEWAY_BREAKER_INTERVAL` | `10s` | Tumbling counter-reset period |
| `GATEWAY_BREAKER_TIMEOUT` | `60s` | Open → half-open recovery |
| `GATEWAY_BREAKER_MAX_REQUESTS` | `5` | Half-open probe budget |
| `GATEWAY_BREAKER_MIN_REQUESTS` | `10` | Minimum volume before tripping |
| `GATEWAY_BREAKER_FAILURE_RATIO` | `0.5` | Failure ratio threshold (0–1] |
| `GATEWAY_BREAKER_RETRY_AFTER` | `60s` | `Retry-After` hint on 503 fast-fail |

## See also

- `internal/proxy/retry` — retry engine that wraps this registry (ADR-0006)
- `internal/proxy/resilience` — composition root
- `internal/config` — `Breaker` struct loaded from env
- ADR-0002: `meta/architecture/decisions/adr/0002-circuit-breaker-volume-based-thresholds.md`
- ADR-0006: `meta/architecture/decisions/adr/0006-proxy-resilience-integration-hybrid.md`
