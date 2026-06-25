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

—
