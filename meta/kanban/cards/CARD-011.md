# CARD-011: Load test, race-suite, CI & make up

**Status:** ready
**Priority:** P2
**Category:** enabler
**Estimate:** 2.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/011-load-test-race-ci-make-up
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-4
**Depends on:** CARD-007, CARD-008, CARD-009, CARD-010
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Deliver the production-readiness harness: load testing, race-free test suite, CI pipeline, docker-compose stack, and README.

**Load test (`load/**`):**
- k6 scenario: ramp → plateau → 2–3× overload; target: gateway p95 overhead ≤ 20ms over the plateau (NFR-001 / AC-042).
- `load/RESULTS.md`: record environment (CPU/RAM, Go version), RPS, p50/p95/p99/error-rate table.
- pprof goroutine-leak check: snapshot before and after the plateau; delta must be within ±5 goroutines (NFR-003 / AC-044).

**Table-driven `-race` suite:**
- Full test suite tagged for `go test -race ./...`; uses testcontainers for real Postgres and Redis instances.
- 0 DATA RACE messages (NFR-008 / AC-049).

**CI (`.github/workflows/ci.yml`, CON-004):**
- GitHub Actions: build + `go test -race ./...` + golangci-lint; mandatory for every PR.
- Lint config (`golangci-lint`) covering at minimum `staticcheck`, `errcheck`, `govet`.

**Docker-compose (`deploy/**`, `Makefile`, CON-005):**
- `make up` brings up: gateway + postgres + redis + prometheus + grafana.
- Grafana dashboard JSON committed to `deploy/grafana/` (panel per each of the 6 required metrics).

**README.md:**
- Headline performance number (p95 overhead from RESULTS.md), architecture diagram (or reference to C4 PUMLs), quickstart (`make up` + `curl`), patterns checklist (rate limiting, retries, circuit breaker, backpressure, cache, metering).

## Acceptance criteria

### NFR-001 — p95 overhead ≤ 20ms

**AC-042**
- **Given:** mock provider with fixed 0ms latency, load of several thousand RPS
- **When:** load test (k6/vegeta) runs for 5 minutes
- **Then:** gateway p95 latency (overhead) <= 20ms across the entire plateau
- **Test:** `BenchGateway_p95OverheadUnder20ms` (kind: boundary)

### NFR-008 — Race-free test suite

**AC-049**
- **Given:** full test suite
- **When:** `go test -race ./...` is run
- **Then:** tests complete with exit code 0, no "DATA RACE" messages
- **Test:** `TestSuite_RaceFree` (kind: happy)

### NFR-002 — Overload graceful degradation (integration via load test)

**AC-043**
- **Given:** load at 3× nominal RPS for 2 minutes
- **When:** load test runs
- **Then:** no panics occur, process uptime is 100%, all responses are 200/429/503; after load drops gateway accepts requests
- **Test:** `BenchGateway_Overload3x_NocrashGracefulDegradation` (kind: boundary)

### NFR-003 — No goroutine leaks (verified in load scenario)

**AC-044**
- **Given:** baseline goroutine count recorded before the load test
- **When:** load test completes and load has subsided
- **Then:** goroutine count equals baseline (tolerance ±5 background goroutines)
- **Test:** `BenchGateway_GoroutineLeakCheck` (kind: boundary)

### NFR-006 — Upstream goroutines bounded (verified in load scenario)

**AC-047**
- **Given:** worker pool limit = 50, load of 500 RPS
- **When:** load test runs for 1 minute
- **Then:** goroutine count on the upstream path never exceeds 50
- **Test:** `BenchGateway_GoroutineCountBounded` (kind: boundary)

## Architecture context

- **FR:** —
- **NFR:** NFR-001, NFR-008
- **CON:** CON-004, CON-005
- **ADR:** —
- **Components:** — (build/load/infra; all prior components must be complete)
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
