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

Implemented on branch `card/006-response-cache`.

**Files**
- `internal/cache/cache.go` — `CacheRepository` port (ACL, ADR-0010): `Get(ctx,key) ([]byte,bool,error)` / `Set(ctx,key,value,ttl) error`.
- `internal/cache/redisrepo.go` — go-redis/v9 adapter (`RedisRepository`) over the narrow `redis.Cmdable` surface; `redis.Nil` → clean miss, other errors surfaced; rejects non-positive TTL; `cache:` key prefix.
- `internal/cache/redisrepo_test.go` — adapter unit tests against a fake `redis.Cmdable` (embeds the interface, overrides only Get/Set; live Redis deferred to CARD-011).
- `internal/middleware/cache.go` — `CacheMiddleware` (depends only on the port).
- `internal/middleware/cache_test.go` — middleware tests with an in-memory fake repo + spy handler.
- `internal/config/config.go` — `Cache{TTL}`; `GATEWAY_CACHE_TTL` (default `5m`, fail-loud, validated `> 0`).
- `cmd/gateway/main.go` — wires `cache.NewRedisRepository(redisClient)` + the middleware.

**Chain position (innermost):** recover → logging → tracing → metrics → rate-limit → counting → **cache** → routes.

**Behaviour**
- Acts ONLY on `POST /v1/chat/completions`; everything else passes through untouched. Nil repo disables caching.
- Reads + restores the request body (`io.NopCloser(bytes.NewReader(body))`) so the downstream strict handler can re-read it.
- `stream:true` (tolerant minimal unmarshal) → full bypass: no key computed, no Get/Set (AC-016).
- Key = `sha256(method \0 path \0 rawBody)` hex. DOCUMENTED limitation: raw-byte keying treats JSON whitespace/key-order differences as distinct entries (acceptable for v1; over-caching is never a correctness risk).
- HIT → write cached 200 + body, `X-Cache: HIT`, `next` NOT called (provider not contacted) — AC-014.
- MISS → capture status+body via an `Unwrap()`-bearing recorder, `X-Cache: MISS` (set before WriteHeader), store only on status 200 — AC-015. Store uses `context.WithoutCancel` + a 2s bound so a completed request's cancellation doesn't abort the best-effort write.
- TTL: default from config; per-request `X-Cache-TTL` (positive integer seconds) overrides for that response only; invalid/non-positive ignored → default (ADR-0004).
- Redis error on Get OR Set → logged at WARN + fall through to the live handler; never a client error (AC-017).

**AC → test mapping**
- AC-014 → `TestCache_Hit_ReturnsCachedResponse`
- AC-015 → `TestCache_Miss_FetchesAndCaches` (asserts Set called with expected TTL)
- AC-016 → `TestCache_StreamingNotCached` (asserts no Get/Set, handler called)
- AC-017 → `TestCache_RedisDown_FallsThrough` (failGet+failSet → 200 from handler)
- Per-request override → `TestCache_PerRequestTTLOverride` (table: 30→30s; abc/0/-5→default)
- Extra: non-target route pass-through, non-200 not cached, body-restored-for-downstream; adapter: Set/Get round-trip, miss, Get/Set backend error, non-positive TTL, key prefix.

**Verification:** `go build ./...` OK; `go test -race ./...` all green; `go generate ./...` diff-clean (api.gen.go / openapi.yaml untouched); `go mod tidy` no changes.
