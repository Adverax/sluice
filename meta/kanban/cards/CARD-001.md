# CARD-001: Service bootstrap & lifecycle

**Status:** done
**Priority:** P1
**Category:** feature
**Estimate:** 2d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/001-service-bootstrap-lifecycle
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-1
**Depends on:** —
**Review score:** 9.0 (3 cycles; 0 critical/important; AC gate all ✓)
**Started:** 2026-06-25T07:20:51Z
**Closed:** 2026-06-25T07:45:57Z
**Actual:** 0.1d
**Merge commit:** 9638bf7
**Blocked by:** —

## What to implement

Bootstrap the bootable service skeleton for sluice. This card covers:

- `internal/config/**.go` — env-based config loading and validation (all values with defaults); statically assert that ReadTimeout, WriteTimeout, IdleTimeout of http.Server; upstream http.Client timeout; Redis dial/read timeout; pgx pool acquire timeout are all > 0 (NFR-004 / AC-045).
- `internal/logging/**.go` — slog structured logger (COMP-015): JSON output in production, per-request middleware that injects `request_id`, `latency_ms`, `status_code` at INFO (FR-016 / AC-040); ERROR level for panics with `panic_value` field (FR-016 / AC-041).
- `internal/lifecycle/**.go` + `cmd/gateway/main.go` — COMP-006 Lifecycle Manager: DI wiring, `signal.NotifyContext(SIGINT/SIGTERM)` → `server.Shutdown` draining in-flight requests, logs `"drained N requests"`, exits with code 0.

ADR-0008: all observability hooks (logger, registry) are injected, not global singletons.

## Acceptance criteria

### FR-016 — Structured logging

**AC-040**
- **Given:** any request to gateway
- **When:** request completes
- **Then:** slog record contains fields `request_id`, `latency_ms`, and `status_code`
- **Test:** `TestLogging_StructuredFieldsPresent` (kind: happy)

**AC-041**
- **Given:** panic in a handler
- **When:** panic recovery fires
- **Then:** log record contains ERROR level and the `panic_value` field
- **Test:** `TestLogging_PanicLoggedAtError` (kind: negative)

### FR-012 — Graceful shutdown (skeleton / drain ACs)

**AC-031**
- **Given:** 5 in-flight requests are active, mock provider responds with 200ms latency
- **When:** process receives SIGTERM
- **Then:** all 5 requests complete successfully; process exits after them with log `"drained N requests"`
- **Test:** `TestGracefulShutdown_DrainsInFlightRequests` (kind: happy)

### NFR-005 — Graceful drain completeness

**AC-046**
- **Given:** 10 in-flight requests, mock with 300ms latency
- **When:** SIGTERM is received
- **Then:** all 10 requests complete successfully; process exit code 0; log contains `"drained 10 requests"`
- **Test:** `TestGracefulShutdown_ZeroDropped` (kind: happy)

### NFR-004 — Timeout coverage

**AC-045**
- **Given:** configuration of the running service
- **When:** ReadTimeout, WriteTimeout, IdleTimeout of http.Server; upstream http.Client timeout; Redis dial/read timeout; pgx pool acquire timeout are checked
- **Then:** all six timeouts are > 0
- **Test:** `TestConfig_AllBoundariesHaveTimeouts` (kind: happy)

## Architecture context

- **FR:** FR-016, FR-012 (shutdown skeleton)
- **NFR:** NFR-005, NFR-004
- **ADR:** ADR-0008
- **Components:** COMP-006 Lifecycle Manager, COMP-015 Structured Logger
- **Trace:** meta/architecture/trace.yml

## Worktree notes

Implemented the bootable service skeleton (stdlib-only, no new deps; `go.mod` unchanged).

Packages/files:
- `internal/config/config.go` — env-based load with defaults + `Validate()`; asserts the 6 AC-045 boundary timeouts (http.Server Read/Write/Idle, upstream client, Redis dial/read, pgx acquire) are all > 0. GATEWAY_ prefix per ADR-0003.
- `internal/logging/logging.go` + `middleware.go` — injected `*slog.Logger` (JSON/text), per-request middleware emitting `request_id`/`latency_ms`/`status_code` at INFO; `LogPanic` records panics at ERROR with `panic_value` (the logging side of AC-041; recovery-as-500 middleware is CARD-009 — middleware re-raises the panic).
- `internal/health/health.go` — `Checker` port + `Handler` with `/healthz` (FR-008) and `/readyz` (FR-009 framework): 503 when any registered checker is unhealthy; CARD-003 registers real Redis/Postgres checkers.
- `internal/lifecycle/lifecycle.go` — COMP-006 Manager: timeout-bearing `http.Server`, `CountingMiddleware` for in-flight tracking, `Run(ctx)` → on ctx cancel does `server.Shutdown`, logs `"drained N requests"`, returns nil (exit 0).
- `cmd/gateway/main.go` — DI wiring (config → logger → health → server → manager → `signal.NotifyContext(SIGINT/SIGTERM)`). No global singletons (ADR-0008).

AC coverage:
- AC-045 (NFR-004) → `TestConfig_AllBoundariesHaveTimeouts` (asserts every timeout > 0).
- AC-040 (FR-016) → `TestLogging_StructuredFieldsPresent`.
- AC-041 (FR-016) → `TestLogging_PanicLoggedAtError` (+ `TestLogPanic_DirectUse`).
- AC-031 (FR-012) → `TestGracefulShutdown_DrainsInFlightRequests` (5 reqs @200ms).
- AC-046 (NFR-005) → `TestGracefulShutdown_ZeroDropped` (10 reqs @300ms, exit 0, "drained 10 requests").
- FR-009 readiness 503 → `TestReady_DependencyUnhealthy_503`.

`go build ./...`, `go vet ./...`, `go test -race ./...` all green. No [BLOCKER].

Scope: proxy/provider/rate-limit/cache/breaker/metering/metrics intentionally NOT implemented (later cards). `WorkerPoolSize` is loaded but the pool is CARD-008.
