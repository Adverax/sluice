# CARD-003: Non-streaming proxy, router, health & timeouts

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 2.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/003-proxy-router-health-timeouts
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-1
**Depends on:** CARD-002
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Implement the non-streaming proxy pipeline, HTTP router, and health/readiness endpoints under `internal/proxy/**.go`, `internal/server/**.go`, and `internal/health/**.go`.

**HTTP Handler & Router (COMP-001):**
- `internal/server/**.go` — construct `http.Server` with explicit ReadTimeout, WriteTimeout, IdleTimeout (all > 0 per NFR-004).
- `internal/proxy/**.go` — HTTP handler + router: parse request body, extract `model` field, route to the correct registered provider; return 400 if body is absent/invalid JSON (AC-004), 400 if `model` field is absent (AC-006), 404 if model is unregistered (AC-007); ADR-0006 (hybrid composition order), ADR-0009 (route through Provider interface).

**Proxy Core — non-streaming path (COMP-002):**
- Call `Provider.Infer(ctx, req)` with the reused `http.Client` (tuned Transport, explicit timeout).
- On provider 500 → 502 JSON error body (AC-003).
- On success → forward response body 200 (AC-001, p95 overhead ≤ 20ms per NFR-001).
- ADR-0010: upstream client timeout configured via env.

**Health & Readiness (COMP-012):**
- `GET /healthz` — always returns 200 with `{"status":"ok"}` (AC-025).
- `GET /readyz` — pings Redis and Postgres; 200 if both up (AC-026); 503 with body `redis:down` if Redis down (AC-027); 503 with body `postgres:down` if Postgres down (AC-028).

## Acceptance criteria

### FR-001 — Proxy (non-streaming)

**AC-001**
- **Given:** valid request with model and messages, provider is available and responds 200
- **When:** API Client sends POST /v1/chat/completions
- **Then:** gateway returns 200 with the provider response body within < upstream latency + 20ms overhead
- **Test:** `TestProxy_HappyPath_NonStreaming` (kind: happy)

**AC-003**
- **Given:** provider returns 500
- **When:** API Client sends POST /v1/chat/completions (retries exhausted)
- **Then:** gateway returns 502 with a JSON error body
- **Test:** `TestProxy_ProviderError_Returns502` (kind: error)

**AC-004**
- **Given:** request body is absent or is not valid JSON
- **When:** API Client sends POST /v1/chat/completions
- **Then:** gateway returns 400 without contacting the provider
- **Test:** `TestProxy_InvalidBody_Returns400` (kind: negative)

### FR-002 — Routing by model field

**AC-005**
- **Given:** two providers are registered in the configuration for different models
- **When:** API Client specifies model "gpt-4"
- **Then:** gateway routes the request to the provider registered for "gpt-4"
- **Test:** `TestRouter_RoutesToCorrectProvider` (kind: happy)

**AC-006**
- **Given:** model field is absent from the request body
- **When:** API Client sends POST /v1/chat/completions
- **Then:** gateway returns 400 without contacting the provider
- **Test:** `TestRouter_MissingModel_Returns400` (kind: negative)

**AC-007**
- **Given:** model field refers to an unregistered model
- **When:** API Client sends POST /v1/chat/completions
- **Then:** gateway returns 404 without contacting the provider
- **Test:** `TestRouter_UnknownModel_Returns404` (kind: negative)

### FR-008 — Liveness

**AC-025**
- **Given:** process is running
- **When:** orchestrator sends GET /healthz
- **Then:** returns 200 OK with body `{"status":"ok"}` or empty
- **Test:** `TestHealthz_ReturnsOK` (kind: happy)

### FR-009 — Readiness

**AC-026**
- **Given:** Redis and Postgres are available
- **When:** operator sends GET /readyz
- **Then:** returns 200 OK with a body including the status of both dependencies
- **Test:** `TestReadyz_AllDepsUp_Returns200` (kind: happy)

**AC-027**
- **Given:** Redis is unavailable (container stopped)
- **When:** operator sends GET /readyz
- **Then:** returns 503 with a body showing `redis:down`
- **Test:** `TestReadyz_RedisDown_Returns503` (kind: negative)

**AC-028**
- **Given:** Postgres is unavailable (container stopped)
- **When:** operator sends GET /readyz
- **Then:** returns 503 with a body showing `postgres:down`
- **Test:** `TestReadyz_PostgresDown_Returns503` (kind: negative)

### NFR-004 — Timeout coverage

**AC-045**
- **Given:** configuration of the running service
- **When:** ReadTimeout, WriteTimeout, IdleTimeout of http.Server; upstream http.Client timeout; Redis dial/read timeout; pgx pool acquire timeout are checked
- **Then:** all six timeouts are > 0
- **Test:** `TestConfig_AllBoundariesHaveTimeouts` (kind: happy)

## Architecture context

- **FR:** FR-001 (non-stream), FR-002, FR-008, FR-009
- **NFR:** NFR-004
- **ADR:** ADR-0006, ADR-0009, ADR-0010
- **Components:** COMP-001 HTTP Handler & Router, COMP-002 Proxy Core, COMP-012 Health & Readiness Handlers
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
