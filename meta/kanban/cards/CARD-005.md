# CARD-005: Per-key rate limiting (local→Redis) + ephemeral key

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 2.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/005-rate-limiting-ephemeral-key
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-2
**Depends on:** CARD-003
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Implement per-API-key token-bucket rate limiting under `internal/ratelimit/**.go` and `internal/middleware/**.go`.

**Rate Limit Middleware (COMP-008):**
- `net/http` middleware; extract API key from `Authorization` header.
- Local token bucket using `golang.org/x/time/rate` per key; distributed enforcement via RateLimitRepository (COMP-009) backed by Redis ACL (ADR-0010).
- **ADR-0001 ephemeral key minting:** if the `Authorization` header is absent, generate a crypto-random ephemeral key (`crypto/rand`), create a fresh per-key bucket, return the ephemeral key via `X-Sluice-Api-Key` response header **and** `Set-Cookie`.
- On limit exceeded: return 429 with `Retry-After` header; do not contact the provider (AC-010, INV-004).
- Within limit: pass request to the next handler (AC-011).
- Missing key handled gracefully: either anonymous bucket or 401 — document the choice in the card Worktree notes once decided (AC-012).

**RateLimitRepository (COMP-009):**
- `internal/ratelimit/**.go` — repository interface + go-redis/v9 implementation; implements distributed global limit check across gateway instances sharing one Redis (AC-013).

ADR-0001: ephemeral_assigned_key — keyless requests get a fresh per-key bucket; key returned to client. ADR-0010: Redis-backed distributed ACL.

## Acceptance criteria

### FR-004 — Per-API-key rate limiting

**AC-010**
- **Given:** API key "key-A" with limit 10 RPS, 10 requests already served within 1 second
- **When:** the 11th request arrives
- **Then:** gateway returns 429 with a Retry-After header and does not contact the provider
- **Test:** `TestRateLimit_ExceedLimit_Returns429WithRetryAfter` (kind: happy)

**AC-011**
- **Given:** API key "key-A" with limit 10 RPS, only 5 requests served within 1 second
- **When:** the next request arrives
- **Then:** gateway passes the request to the provider without 429
- **Test:** `TestRateLimit_WithinLimit_Passes` (kind: happy)

**AC-012**
- **Given:** Authorization header is absent from the request
- **When:** a request arrives at POST /v1/chat/completions
- **Then:** gateway applies rate limiting for the anonymous key or returns 401
- **Test:** `TestRateLimit_MissingApiKey_HandledGracefully` (kind: negative)

**AC-013**
- **Given:** two gateway instances share one Redis, API key with a shared limit of 100 RPS
- **When:** 60 requests arrive on the first instance and 60 on the second simultaneously
- **Then:** no more than 100 requests total pass per second; the rest receive 429
- **Test:** `TestRateLimit_DistributedRedis_GlobalLimit` (kind: boundary)

## Architecture context

- **FR:** FR-004
- **NFR:** —
- **ADR:** ADR-0001, ADR-0010
- **Components:** COMP-008 Rate Limit Middleware, COMP-009 RateLimitRepository
- **Trace:** meta/architecture/trace.yml

## Worktree notes

### Implementation summary

Per-API-key rate limiting implemented as a two-tier net/http middleware
(COMP-008) + a distributed repository port (COMP-009).

**Files added/changed:**
- `internal/ratelimit/ratelimit.go` — `RateLimitRepository` port (the ACL,
  ADR-0010), per-key LOCAL token-bucket `Registry` over `golang.org/x/time/rate`
  (injectable clock for deterministic tests via `ReserveN`/`DelayFrom`/`CancelAt`).
- `internal/ratelimit/memrepo.go` — `MemoryRepository`: in-memory fixed-window
  global counter; used as the SHARED store in the AC-013 test (and a safe
  single-instance default).
- `internal/ratelimit/redisrepo.go` — `RedisRepository`: go-redis/v9 adapter
  enforcing an ATOMIC fixed-window cap via a Lua INCR+PEXPIRE+PTTL script
  (`redis.Scripter`, so *redis.Client/Ring/Cluster all satisfy it).
- `internal/middleware/ratelimit.go` — the `RateLimiter` middleware.
- `internal/config/config.go` — `RateLimit{RPS,Burst,Window}` (env
  `GATEWAY_RATELIMIT_RPS` default 10, `GATEWAY_RATELIMIT_BURST` default 20,
  `GATEWAY_RATELIMIT_WINDOW` default 1s), fail-loud validation.
- `cmd/gateway/main.go` — wiring (see chain below); reuses the existing
  `*redis.Client`.
- Tests: `internal/middleware/ratelimit_test.go`,
  `internal/ratelimit/ratelimit_test.go`, `internal/ratelimit/redisrepo_test.go`.

**Middleware chain position (ADR-0006), outermost first:**
`logging → rate-limit → counting → generated routes(+OpenAPI validation)`.
The rate-limit middleware is OUTERMOST of the request work: a 429 is returned
before the counting middleware / proxy / worker-pool / provider run (INV-004).

**How the two tiers work:** a request must pass BOTH tiers. Tier 1 is the LOCAL
per-key token bucket (fast, no network) bounding per-instance burst. Tier 2 is
the DISTRIBUTED `RateLimitRepository` (atomic Redis fixed-window) enforcing one
GLOBAL cap shared across all instances pointing at the same Redis (AC-013). The
middleware depends only on the port, never on go-redis directly.

### AC-012 ephemeral-key decision (ADR-0001)

Chosen strategy: **ephemeral_assigned_key** (NOT 401, NOT a single shared
anonymous bucket). When `Authorization` is absent the middleware mints a
crypto/rand ephemeral key (`eph_<hex>`, never math/rand), creates a fresh
per-key bucket, advertises the key on the response via BOTH the
`X-Sluice-Api-Key` header AND a `Set-Cookie` (`sluice_api_key=...; HttpOnly`),
and rate-limits the request under that minted key. The header/cookie are set
even on a 429 so the client always learns the key to reuse. This is how AC-012
"handled gracefully" is satisfied and gives every anonymous client an isolated
bucket (no noisy-neighbour). Eviction of idle per-key buckets is a documented
follow-up (deferred per ADR-0001 "Negative") — v1 does not evict.

### Redis fail-open vs fail-closed (resilience)

**Chosen: FAIL-OPEN.** If the distributed repository returns an error (e.g.
Redis unreachable), the middleware logs a WARN and falls back to the LOCAL
limiter verdict rather than rejecting. Rationale: a proxy's job is availability;
fail-closed would amplify a Redis blip into a fleet-wide outage. The local
token bucket still bounds per-instance burst during the degradation.

### Notes

- The real go-redis adapter is unit-tested here against a fake `redis.Scripter`
  (reply parsing + fail-open error path); a LIVE Redis is integration-tested in
  CARD-011 via testcontainers — not required in this unit suite.
- `go build ./...` + `go test -race ./...` green; `go generate ./...`
  diff-clean (internal/api & api/openapi.yaml untouched); `go mod tidy` run
  (golang.org/x/time added as a direct dependency).

### AC → test mapping

| AC | Test |
|----|------|
| AC-010 | `TestRateLimit_ExceedLimit_Returns429WithRetryAfter` (spy asserts provider NOT called on 429) |
| AC-011 | `TestRateLimit_WithinLimit_Passes` |
| AC-012 | `TestRateLimit_MissingApiKey_HandledGracefully` (X-Sluice-Api-Key + HttpOnly Set-Cookie + fresh bucket) |
| AC-013 | `TestRateLimit_DistributedRedis_GlobalLimit` (shared MemoryRepository, two middleware instances, 60+60 → exactly 100 pass) |
| fail-open | `TestRateLimit_DistributedFailOpen` |
