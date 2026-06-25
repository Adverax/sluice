# internal/proxy/retry

## Purpose

Implements COMP-003 — the retry engine (FR-006). Wraps a provider call with bounded,
deadline-aware exponential backoff + jitter, retrying only transient failures. Non-retryable
errors (4xx, `context.Canceled`/`DeadlineExceeded`, open-breaker sentinel) are returned
immediately without consuming the retry budget.

## Architecture

The engine operates on a generic `Call` func — `func(ctx) (provider.Response, error)` —
not on a concrete provider type. This is the same shape the circuit breaker (internal/breaker)
wraps, enabling ADR-0006 composition without either layer knowing about the other's internals:

```
retry.Engine.Do(ctx, guarded)
    └─ guarded = breaker.Registry.Execute(ctx, key, providerCall)
```

Error classification uses typed/sentinel errors — never string-matching. A
`*provider.StatusError` with a 5xx code is retryable; 4xx is not (AC-021). A caller-supplied
`nonRetryable` predicate marks `gobreaker.ErrOpenState` non-retryable (ADR-0006), preventing
the retry loop from spinning against an open breaker.

Backoff formula: `BaseDelay × 2^(attempt-1)`, capped at `MaxDelay`, minus up to `Jitter`
fraction applied randomly to spread retries (avoids thundering herd).

## Key types

| Type | Description |
|------|-------------|
| `Engine` | Stateless retry engine; safe for concurrent use. Construct with `New`. |
| `Call` | `func(ctx context.Context) (provider.Response, error)` — the unit of work. |
| `ErrExhausted` | Sentinel wrapping the last error after all attempts are spent; server maps it to 502. |
| `Option` | Functional option for `New` (`WithNonRetryable`, `WithSleep`, `WithRand`). |

`WithSleep` and `WithRand` are injectable for deterministic tests (no real delays, no RNG).

## Environment variables

Populated via `internal/config.Retry` from `config.Load()`.

| Variable | Default | Description |
|----------|---------|-------------|
| `GATEWAY_RETRY_MAX_ATTEMPTS` | `3` | Total attempts (first call + retries); `1` disables retries |
| `GATEWAY_RETRY_BASE_DELAY` | `50ms` | Backoff for the first retry |
| `GATEWAY_RETRY_MAX_DELAY` | `2s` | Cap on exponential backoff |
| `GATEWAY_RETRY_JITTER` | `0.5` | Jitter fraction in `[0,1]` |

## See also

- `internal/breaker` — circuit breaker wrapped by this engine (ADR-0006)
- `internal/proxy/resilience` — composition root that wires retry + breaker
- `internal/provider` — `StatusError.Retryable()` drives retry classification
- `internal/config` — `Retry` struct loaded from env
- ADR-0006: `meta/architecture/decisions/adr/0006-proxy-resilience-integration-hybrid.md`
