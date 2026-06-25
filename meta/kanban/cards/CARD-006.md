# CARD-006: Response cache (Redis, TTL + per-request override)

**Status:** ready
**Priority:** P2
**Category:** feature
**Estimate:** 1.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/006-response-cache-redis-ttl
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

Implement the Redis response cache under `internal/cache/**.go` as COMP-004 Cache Adapter (CacheRepository).

- **CacheRepository ACL** over `go-redis/v9` (ADR-0010): define a `CacheRepository` interface with `Get(ctx, key) ([]byte, error)` and `Set(ctx, key, value, ttl) error`; implement with go-redis/v9.
- **Default TTL:** 5 minutes (ADR-0004). Per-request override via `X-Cache-TTL` request header (value in seconds); if the header is present and valid, override the TTL for that response only.
- **Cache key:** hash of the canonical request body (method + path + body bytes); streaming requests (`"stream":true`) must **not** generate a cache key — bypass cache entirely (AC-016).
- **Cache hit:** return cached response body with header `X-Cache: HIT`, do not contact provider (AC-014).
- **Cache miss:** proxy to provider, store response, return with `X-Cache: MISS` (AC-015).
- **Redis down:** fall through — serve live from provider without error; do not propagate Redis errors to the client (AC-017).

ADR-0004: default_5min_per_request_override — default TTL is 5 minutes; `X-Cache-TTL` header overrides per request. ADR-0010: cache repository is an interface; Redis is injected.

## Acceptance criteria

### FR-005 — Response cache

**AC-014**
- **Given:** a request with body `{"model":"mock","messages":[...]}` has been executed and cached
- **When:** an identical request arrives within the TTL
- **Then:** gateway returns the cached response with header `X-Cache: HIT` without contacting the provider
- **Test:** `TestCache_Hit_ReturnsCachedResponse` (kind: happy)

**AC-015**
- **Given:** cache is empty or TTL has expired
- **When:** a request arrives
- **Then:** gateway contacts the provider and caches the new response; returns `X-Cache: MISS`
- **Test:** `TestCache_Miss_FetchesAndCaches` (kind: happy)

**AC-016**
- **Given:** request has `"stream":true`
- **When:** the request arrives
- **Then:** streaming responses are not cached (cache key is not computed)
- **Test:** `TestCache_StreamingNotCached` (kind: negative)

**AC-017**
- **Given:** Redis is unavailable
- **When:** a request arrives
- **Then:** gateway bypasses the cache and proxies to the provider without error
- **Test:** `TestCache_RedisDown_FallsThrough` (kind: error)

## Architecture context

- **FR:** FR-005
- **NFR:** —
- **ADR:** ADR-0004, ADR-0010
- **Components:** COMP-004 Cache Adapter (CacheRepository)
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
