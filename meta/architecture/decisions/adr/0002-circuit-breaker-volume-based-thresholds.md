# ADR-0002: Circuit Breaker Thresholds (Volume-Based, 50%)

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

The gateway protects upstream providers with a circuit breaker (FR-007, library `sony/gobreaker`). Aggregate `AGG-003` in CTX-002 (Resilience) manages `CircuitBreakerState`. Invariant INV-006 requires the circuit breaker to open upon reaching the error threshold. Invariant INV-005 requires fast-fail in the `open` state.

Without specific numeric thresholds, acceptance criteria AC-022 and AC-023 (FR-007) are not executable tests — the behaviour cannot be reproduced reliably in CI. The choice of thresholds affects stability under load (NFR-002): an overly aggressive threshold causes false positives during brief error spikes.

## Decision

We adopt the `volume_based_50pct` strategy: the circuit breaker opens when the error rate reaches ≥ 50% within a 10-second window with a minimum volume of at least 10 requests; the recovery timeout is 60 seconds, after which the breaker transitions to `half-open` for probe requests. The minimum volume of 10 requests prevents tripping on isolated errors at startup. This makes AC-022 and AC-023 concrete and reproducible in a test environment with a mock provider.

**Important note on window semantics:** `sony/gobreaker` implements a **tumbling window** out of the box (there is no sliding window). The `Interval` parameter sets the counter-reset period — every `Interval` seconds the counters are zeroed. This is not a sliding window in the sense of Envoy outlier detection or resilience4j. We state this honestly: the word "sliding" has been replaced with "window" in the text above.

**Concrete `gobreaker.Settings` configuration:**

```go
gobreaker.Settings{
    Interval:    10 * time.Second, // tumbling counter reset every 10s
    Timeout:     60 * time.Second, // open → half-open after 60s
    MaxRequests: 5,                // probe requests in half-open; first success → closed, any failure → open
    ReadyToTrip: func(counts gobreaker.Counts) bool {
        return counts.Requests >= 10 &&
            float64(counts.TotalFailures)/float64(counts.Requests) >= 0.5
    },
}
```

`MaxRequests: 3–5` is an acceptable range; 5 was chosen to provide sufficient statistics in half-open without excessive traffic to the recovering provider.

## Alternatives considered

### consecutive_5

5 consecutive errors → breaker transitions to `open`; recovery timeout 30 seconds. The simple default `gobreaker` configuration. Rejected because the approach is less robust against brief spikes: 5 consecutive errors can occur even when the provider is functioning normally with rare failures. For production workloads this threshold is too aggressive and causes false positives.

## Consequences

### Positive
- Thresholds are concrete and reproducible: AC-022 (`TestCircuitBreaker_OpenState_FastFail`) and AC-023 (`TestCircuitBreaker_ThresholdExceeded_Opens`) become executable tests with specific numbers.
- The volume-based approach is resilient to brief spikes and isolated errors — no false positives under normal traffic with rare failures (NFR-002).
- A 60-second timeout gives the upstream provider sufficient time to recover before the next probe request (AC-024).

### Negative
- The minimum threshold of 10 requests means that at low traffic (< 10 req/10s) the circuit breaker will not open even at 100% errors. On test and dev environments with low load this may conceal problems.
- The configuration is more complex than `consecutive_5`: three parameters (window, minimum requests, error percentage) instead of one.
- **Tumbling window — not a sliding window.** A burst of errors arriving at the boundary of two `Interval` periods will be split across two windows and may briefly under-count or over-count depending on phase. A true sliding window (as in Envoy outlier detection or resilience4j sliding-window) would require a custom implementation. For this PoC a tumbling window is **acceptable** — provided it is explicitly documented (which this ADR does).

### Neutral
- Parameters are configurable via env: GATEWAY_BREAKER_INTERVAL, GATEWAY_BREAKER_TIMEOUT, GATEWAY_BREAKER_MAX_REQUESTS, GATEWAY_BREAKER_MIN_REQUESTS, GATEWAY_BREAKER_FAILURE_RATIO, GATEWAY_BREAKER_RETRY_AFTER (all with the defaults stated above; fail-loud on invalid values).
- The `sony/gobreaker` library natively supports volume-based configuration via the `ReadyToTrip` callback — no additional dependencies are required (CON-001, CON-002).

## References

- DEC-002 (resolved by this ADR)
- CTX-002 (Resilience — AGG-003, CircuitBreakerState)
- FR-007 (circuit breaker, AC-022, AC-023, AC-024)
- NFR-002 (availability under overload)
- INV-005, INV-006 (fast-fail and threshold invariants)

## History

- 2026-06-25: Created — volume-based 50% over 10s with a minimum of 10 requests, recovery timeout 60s; makes AC-022/AC-023 executable tests.
