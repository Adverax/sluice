# CARD-003: Non-streaming proxy, router, health & timeouts

**Status:** done
**Priority:** P1
**Category:** feature
**Estimate:** 2.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/003-proxy-router-health-timeouts
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-1
**Depends on:** CARD-002, CARD-012
**Review score:** 9.0 (2 cycles; cycle-1 important: OpenAPI validation now enforced; AC gate all ✓)
**Started:** 2026-06-25T08:40:15Z
**Closed:** 2026-06-25T09:09:52Z
**Actual:** 0.1d
**Merge commit:** 9593466
**Blocked by:** —

## What to implement

**Contract-first (ADR-0011):** CARD-012 produces `api/openapi.yaml` and the generated
server (`internal/api/`: request/response types + `StrictServerInterface` + `net/http`
router registration). This card **implements that generated interface** — do NOT hand-roll
DTOs or routing that the spec already defines; map the generated API types ↔ `provider.Request`/`Response`.

Implement the non-streaming proxy pipeline, the generated-server implementation, and
health/readiness logic under `internal/proxy/**.go`, `internal/server/**.go`, and `internal/health/**.go`.

**HTTP Handler & Router (COMP-001):**
- `internal/server/**.go` — construct `http.Server` with explicit ReadTimeout, WriteTimeout, IdleTimeout (all > 0 per NFR-004); mount the generated `internal/api` router.
- Implement the generated `StrictServerInterface`: the `POST /v1/chat/completions` operation maps the generated request type → `provider.Request`, routes by `model` to the registered provider, and maps `provider.Response` → the generated response type. Body decoding/validation and 400-on-malformed are handled by the generated strict server (AC-004); return 400 if `model` absent (AC-006), 404 if model unregistered (AC-007); ADR-0006 (hybrid composition order), ADR-0009 (route through Provider interface).

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

Implemented on branch `card/003-proxy-router-health-timeouts` (golang-pro).

**Packages created**
- `internal/proxy/` — `Router` registry mapping model→`provider.Provider` (FR-002);
  `ErrModelNotRegistered` (→404). Ports-and-adapters seam; no concrete provider import.
- `internal/server/` — implements the generated `api.StrictServerInterface` (ADR-0011).
  `CreateChatCompletion` maps `api.ChatCompletionRequest`→`provider.Request`
  (temperature float32→*float64, role enum, stream/max_tokens), routes by model, calls
  the provider via a swappable `InferFunc` hook (ADR-0006 seam for FR-007 retry/breaker),
  maps `provider.Response`→`api.ChatCompletionResponse`. `GetHealthz`/`GetReadyz`/`GetMetrics`
  also implemented. `Server.Handler(mux)` wires `api.NewStrictHandler` + `api.HandlerFromMux`.

**Packages changed**
- `internal/health/` — added `Result` + `Evaluate(ctx)` (transport-agnostic readiness
  verdict; `Handler.Ready` now delegates to it so the body matches the spec schema).
  Added `checkers.go`: `NewRedisChecker` (go-redis Ping) + `NewPostgresChecker` (pgx Ping)
  over narrow `RedisPinger`/`PostgresPinger` ports.
- `cmd/gateway/main.go` — DI wiring: proxy.Router (Mock registered for "mock" until real
  adapters land), reused tuned `http.Client` w/ explicit timeout (ADR-0010), real go-redis
  client + pgx pool built from config DSNs with config timeouts (NFR-004), registered as
  readiness checkers. Composition order (ADR-0006): logging → counting → generated routes.

**Wiring the generated interface**: `server.Server` satisfies `api.StrictServerInterface`
(compile-time assertion); all four routes registered via `api.HandlerFromMux` on a
`*http.ServeMux` (CON-001, no framework). JSON decode + malformed-body→400 handled by the
generated strict server (verified by test). Did not edit `api.gen.go`/`openapi.yaml`.

**Deps added**: `github.com/redis/go-redis/v9`, `github.com/jackc/pgx/v5` (+ `go mod tidy`).
Note: `go mod tidy` canonicalizes the go directive to `go 1.25.0` (semantically == 1.25).

**AC→test mapping**
- AC-001 `TestProxy_HappyPath_NonStreaming` · AC-003 `TestProxy_ProviderError_Returns502`
- AC-004 `TestProxy_InvalidBody_Returns400` · AC-005 `TestRouter_RoutesToCorrectProvider`
  (+ proxy-pkg `TestRouter_RoutesToCorrectProvider`, two providers)
- AC-006 `TestRouter_MissingModel_Returns400` · AC-007 `TestRouter_UnknownModel_Returns404`
  (400/404 assert provider spy NOT called)
- AC-025 `TestHealthz_ReturnsOK` · AC-026 `TestReadyz_AllDepsUp_Returns200`
- AC-027 `TestReadyz_RedisDown_Returns503` · AC-028 `TestReadyz_PostgresDown_Returns503`
  (readyz tests use STUB checkers via health.Checker port — no containers)
- AC-045 `TestConfig_AllBoundariesHaveTimeouts` (unchanged, green)
- Real checker mapping unit-tested with fakes: `TestRedisChecker`, `TestPostgresChecker`.

**Results**: `go build ./...` OK; `go test -race ./...` all green; `go generate ./...`
diff-clean (no changes to internal/api or api/openapi.yaml).
