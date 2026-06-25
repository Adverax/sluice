# CARD-001: Service bootstrap & lifecycle

**Status:** ready
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
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
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

—
