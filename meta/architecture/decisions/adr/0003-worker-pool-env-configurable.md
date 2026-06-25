# ADR-0003: Worker Pool Size is Env-Configurable

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

The gateway limits concurrency to upstream providers via a bounded worker pool (FR-015). NFR-006 requires that the number of goroutines waiting for an upstream response must strictly not exceed the pool size. Without a concrete number, acceptance criteria AC-038 (`TestWorkerPool_Saturated_Returns503WithRetryAfter`), AC-039, and AC-047 have no specific threshold — the tests are not executable.

Constraint CON-001 requires a stdlib-first approach; CON-003 fixes a single Go service without a microservices architecture. The pool size depends on the available hardware and the characteristics of the upstream provider, making a hard-coded value inconvenient for operational tuning.

## Decision

We adopt the `env_configurable` strategy: the worker pool size is read from the environment variable `GATEWAY_WORKER_POOL_SIZE` at service startup, with a default value of 100. This satisfies NFR-006 (a concrete upper bound for goroutines on the upstream path) and provides operational flexibility without rebuilding the binary.

## Alternatives considered

### fixed_100

The worker pool size is hard-coded as a constant (value 100). Simplicity and reproducibility of load tests. Rejected because it does not allow adaptation to different hardware configurations and environment requirements without recompilation. On resource-rich machines a limit of 100 may be suboptimal.

## Consequences

### Positive
- AC-038, AC-039, AC-047 become executable: tests can set a specific value via env and verify behaviour when the pool is saturated.
- Operational flexibility: pool size is configurable via env without a rebuild — important when deploying on different node types.
- NFR-006 is satisfied: the upper bound on goroutines on the upstream path is defined and controlled.

### Negative
- Minor risk of misconfiguration: an operator may set a value that is too large or too small. Validation of the value at startup is required (> 0, a reasonable maximum).
- Slightly more configuration: one env variable is added to the service configuration schema.

### Neutral
- The default value of 100 is used in tests and documentation as the reference number for load test calculations.
- `make up` (CON-005) should include `GATEWAY_WORKER_POOL_SIZE=100` in docker-compose.yml as an explicit default for reproducibility.
- Reading from env via `os.Getenv` / `strconv.Atoi` is stdlib — no additional dependencies (CON-001).

## References

- DEC-003 (resolved by this ADR)
- CTX-001 (Proxy — owns the worker pool)
- FR-015 (bounded worker pool, AC-038, AC-039)
- NFR-006 (goroutine count bounded)
- CON-001 (stdlib-first)

## History

- 2026-06-25: Created — worker pool size is read from `GATEWAY_WORKER_POOL_SIZE` (default 100), making AC-038/AC-039/AC-047 executable tests.
