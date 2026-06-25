# CARD-011: Load test, race-suite, CI & make up

**Status:** done
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
**Review score:** 9.5 (2 cycles; cycle-1 important: make run/up :8080 conflict, fixed; ACs ✓, integration suite live-green)
**Started:** 2026-06-25T12:32:46Z
**Closed:** 2026-06-25T13:04:51Z
**Actual:** 0.1d
**Merge commit:** b0e400b
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

**CARD-011 implemented (branch card/011-load-test-ci-makeup).**

**Integration suite (`internal/integration/`, build tag `integration`):**
testcontainers-go spins up REAL Postgres + Redis and exercises the deferred
paths: metering pgx batch-INSERT + read-back (migration applied), cache Redis
Set/Get/TTL round-trip, distributed rate-limit Lua across two repo instances
sharing one Redis (AC-013), and readiness 200→503 when a container is stopped.
Ran here against live Docker — **all 4 PASS, race-clean**:
`go test -tags=integration -race -p 1 ./...` → `ok internal/integration 7.3s`.
Degrades to skip-with-log when no Docker daemon is reachable.

**Load (`load/`):** `load/bench_test.go` is the in-process AC harness over the
full server handler chain with a 0-latency mock — `TestBenchGateway_p95Overhead-
Under20ms` (AC-042), `_Overload3x_…` (AC-043), `_GoroutineLeakCheck` (AC-044),
`_GoroutineCountBounded` (AC-047), `TestSuite_RaceFree` (AC-049). All PASS.
**Real p95 overhead ≈ 11 µs (normal) / 67 µs (-race), Apple M5 Pro / Go 1.26 —
~1000× under the 20 ms NFR-001 budget.** `load/scenario.js` is the k6 ramp →
plateau (~3k RPS) → 3× spike → recovery scenario with NFR thresholds baked in.
`load/RESULTS.md` carries the REAL in-process numbers (labelled with the env) and
leaves the full-stack k6 table as an explicit `TODO: measure via make load`
(k6 not installed in this environment — no fabricated figures).

**CI (`.github/workflows/ci.yml`, CON-004):** jobs build (+ go-generate diff
check), unit+race, golangci-lint, and a testcontainers integration job.
`.golangci.yml` enables staticcheck/errcheck/govet/ineffassign/unused/gofmt.
Fixed the pre-existing SA1019 (`api.GetSwagger` → non-deprecated `api.GetSpec`
in `internal/server/server.go`) and removed an unused test helper that `unused`
flagged. `golangci-lint run` is clean (exit 0).

**Make-up stack (CON-005):** multi-stage `Dockerfile` (distroless final image);
`deploy/docker-compose.stack.yml` adds gateway + one-shot `migrate` + prometheus
+ grafana, layered on top of the managed `deploy/docker-compose.yml` so the
forge:harness managed region is untouched (`docker compose config` validates).
`deploy/prometheus.yml` scrapes the gateway; `deploy/grafana/provisioning/` +
`deploy/grafana/dashboards/sluice.json` auto-provision a dashboard with a panel
per metric. Makefile gains `up` (full stack, overrides managed up BELOW the
markers), `down`, `migrate`, `stack-logs`, `test-integration`, `load`.

**README.md:** headline performance line, C4 links, `make up` quickstart with
non-stream + streaming curl examples, and a patterns→package/file checklist.

**Gates:** `go build ./...` OK; `go test -race ./...` green; integration suite
PASS; `golangci-lint run` clean; `go generate ./...` diff-clean (api.gen.go /
openapi.yaml untouched); `go mod tidy` applied (added testcontainers-go +
modules/postgres + modules/redis). k6 not installed locally → `make load`
artifact provided with run instructions instead of a local k6 run.
