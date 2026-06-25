# CARD-007: Retries & circuit breaker

**Status:** done
**Priority:** P1
**Category:** feature
**Estimate:** 2.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/007-retries-circuit-breaker
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-3
**Depends on:** CARD-003
**Review score:** 9.0 (2 cycles; cycle-1 important: fail-loud config; 7 ACs ✓)
**Started:** 2026-06-25T09:17:19Z
**Closed:** 2026-06-25T09:45:03Z
**Actual:** 0.1d
**Merge commit:** e6a98db
**Blocked by:** —

## What to implement

Implement retry engine and circuit breaker under `internal/proxy/retry/**.go` and `internal/breaker/**.go`.

**Retry Engine (COMP-003):**
- Exponential backoff with jitter; bounded retry count (configurable, default 3 attempts total).
- Deadline-aware: before each retry attempt check `ctx.Err()`; if deadline exceeded, do not start the retry — return 499/503 with cancellation information (AC-020).
- Retry **only** on safe/transient errors (5xx, network errors); do **not** retry 4xx client errors (AC-021).
- Do **not** retry if the error is `gobreaker.ErrOpenState` — fast-fail immediately to 503 + Retry-After.

**Circuit Breaker (COMP-011):**
- Per-provider `sony/gobreaker` instance.
- Tuning per ADR-0002 (volume_based_50pct): `Interval` (tumbling window) = 10s, `ReadyToTrip`: `counts.Requests >= 10 && failureRatio >= 0.5`, `Timeout` (open→half-open) = 60s, `MaxRequests` (probes in half-open) = 3–5.
- Open state → `ErrOpenState` → no retry → 503 + `Retry-After` header (fast-fail, latency < 1ms, AC-022, INV-005).
- Threshold exceeded → transitions to open; subsequent requests get EVT-004 (AC-023, INV-006, POL-002).
- Half-open + successful probe → transitions to closed; response returned to client (AC-024).

**Composition (ADR-0006):** `retry(breaker.Execute(providerCall))` — breaker wraps the single provider call; retry wraps the breaker call. On `ErrOpenState` the retry layer treats it as non-retryable and propagates immediately.

## Acceptance criteria

### FR-006 — Retries with exponential backoff

**AC-018**
- **Given:** provider returns 503 on the first 2 requests, then 200
- **When:** API Client sends a request
- **Then:** gateway performs 2 retries and returns the successful response to the client
- **Test:** `TestRetry_TransientError_SucceedsOnThirdAttempt` (kind: happy)

**AC-019**
- **Given:** provider consistently returns 503, maximum attempts = 3
- **When:** API Client sends a request
- **Then:** gateway returns 502 after exhausting all attempts
- **Test:** `TestRetry_ExhaustedAttempts_Returns502` (kind: negative)

**AC-020**
- **Given:** client context deadline expires during a retry
- **When:** gateway attempts to perform the retry
- **Then:** retry does not start; 499 or 503 is returned with cancellation information
- **Test:** `TestRetry_ContextDeadlineExpired_NoRetry` (kind: boundary)

**AC-021**
- **Given:** provider returns 400 (client error, not idempotent)
- **When:** API Client sends a request
- **Then:** gateway does not retry and immediately returns the provider's response
- **Test:** `TestRetry_ClientError_NoRetry` (kind: negative)

### FR-007 — Circuit breaker

**AC-022**
- **Given:** circuit breaker is in open state for provider P
- **When:** a request arrives for provider P
- **Then:** gateway immediately returns 503 without contacting P; latency < 1ms
- **Test:** `TestCircuitBreaker_OpenState_FastFail` (kind: happy)

**AC-023**
- **Given:** provider P returns errors exceeding the breaker threshold
- **When:** gateway records the next error
- **Then:** circuit breaker transitions to open state; subsequent requests receive EVT-004
- **Test:** `TestCircuitBreaker_ThresholdExceeded_Opens` (kind: happy)

**AC-024**
- **Given:** circuit breaker is in open state, recovery timeout has elapsed
- **When:** a probe request arrives in half-open state
- **Then:** provider responds successfully → circuit transitions to closed; response is returned to client
- **Test:** `TestCircuitBreaker_HalfOpen_SuccessClosesCircuit` (kind: boundary)

## Architecture context

- **FR:** FR-006, FR-007
- **NFR:** —
- **ADR:** ADR-0002, ADR-0006
- **Components:** COMP-003 Retry Engine, COMP-011 Circuit Breaker
- **Trace:** meta/architecture/trace.yml

## Worktree notes

Implemented in worktree `card/007-retries-circuit-breaker`.

**Packages / files**
- `internal/proxy/retry/retry.go` — Retry Engine (COMP-003). Bounded exponential
  backoff + jitter, deadline-aware (`ctx.Err()` checked before every attempt and
  during backoff), classification via typed/sentinel errors (no string-matching):
  `*provider.StatusError` 5xx → retryable, 4xx → not; ctx errors → not; injectable
  `WithNonRetryable` predicate marks `gobreaker.ErrOpenState` non-retryable. Sleep
  and jitter RNG are injectable (`WithSleep`, `WithRand`) for deterministic tests.
  Exhausted → wraps `ErrExhausted`.
- `internal/breaker/breaker.go` — Circuit Breaker (COMP-011). Per-provider
  `sony/gobreaker` registry keyed by model/provider name. ADR-0002 tuning
  (Interval 10s, Timeout 60s, MaxRequests 5, ReadyToTrip Requests≥10 &&
  ratio≥0.5), all configurable. `WithSettings` makes timing injectable so the
  half-open test uses a 20ms Timeout instead of 60s. `WithOnStateChange` surfaces
  EVT-004.
- `internal/proxy/resilience/resilience.go` — composition root (ADR-0006):
  `retry(breaker.Execute(providerCall))`, keyed by `req.Model`. Maps ErrOpenState
  / deadline → `*Unavailable` (→ server 503 + Retry-After); exhausted/4xx → 502.
- `internal/provider/provider.go` — added `StatusError{Code,Message}` typed error
  with `Retryable()` (5xx) for adapter/Mock classification.
- `internal/server/server.go` — added `ErrServiceUnavailable` sentinel +
  Retry-After-bearing 503 response wrapper; `CreateChatCompletion` maps the
  resilience signal to 503, everything else to 502. Seam unchanged (`InferFunc`).
- `internal/config/config.go` — `Retry` + `Breaker` config (env
  `GATEWAY_RETRY_*` / `GATEWAY_BREAKER_*`) with ADR defaults + validation.
- `cmd/gateway/main.go` — wires `retry.New` + `breaker.NewRegistry` into
  `resilience.New(...).InferFunc()` injected via `server.WithInferFunc`.

**Composition into the InferFunc seam (ADR-0006):**
`server.CreateChatCompletion` → `s.infer` (= composed InferFunc) →
`retry.Do(breaker.Execute(provider.Infer))`. ErrOpenState is non-retryable
(propagated immediately → 503). Seam left clean for CARD-008 (worker pool wraps
the same `InferFunc` signature).

**Deps:** `github.com/sony/gobreaker v1.0.0`.

**AC → test mapping**
- AC-018 `TestRetry_TransientError_SucceedsOnThirdAttempt` + integration
  `TestComposition_TransientThenSuccess_200`
- AC-019 `TestRetry_ExhaustedAttempts_Returns502` + `TestComposition_ExhaustedRetries_502`
- AC-020 `TestRetry_ContextDeadlineExpired_NoRetry` (+`_DeadlineDuringBackoff_NoRetry`)
  + `TestComposition_DeadlineExpired_503`
- AC-021 `TestRetry_ClientError_NoRetry` + `TestComposition_ClientError_NoRetry_502`
- AC-022 `TestCircuitBreaker_OpenState_FastFail` (no provider call, <1ms) +
  `TestComposition_BreakerOpen_503_RetryAfter`
- AC-023 `TestCircuitBreaker_ThresholdExceeded_Opens` (incl. min-volume guard +
  EVT-004 hook)
- AC-024 `TestCircuitBreaker_HalfOpen_SuccessClosesCircuit` (injected 20ms Timeout)

**Verification:** `go build ./...`, `go vet ./...`, `go test -race ./...` all
green; `go generate ./...` diff-clean (internal/api + api/openapi.yaml
untouched); `go mod tidy` applied.
