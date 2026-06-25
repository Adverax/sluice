# internal/cache

Response-cache ACL (COMP-004, FR-005) — the `CacheRepository` port and its Redis adapter
(ADR-0010). The cache *middleware* lives in `internal/middleware/cache.go`; this package is
just the storage boundary.

## Port

```go
type CacheRepository interface {
    Get(ctx, key) (value []byte, found bool, err error)
    Set(ctx, key string, value []byte, ttl time.Duration) error
}
```

`RedisRepository` implements it over the narrow `redis.Cmdable` surface (so `*redis.Client`,
`Ring`, `Cluster` all satisfy it; `redis.Nil` → clean miss; non-positive TTL rejected;
`cache:` key namespace). The middleware depends only on the interface — Redis is injected.

## Behavior (the middleware)

- Acts only on `POST /v1/chat/completions`; default TTL 5 min (`GATEWAY_CACHE_TTL`),
  per-request override via `X-Cache-TTL` (positive seconds; invalid → default).
- Key = `sha256(method \0 path \0 rawBody)` — **intentionally cross-tenant** (identical body →
  identical completion; the cached body is not user-specific).
- Stores an envelope `{ct, body}` so a HIT replays the same `Content-Type` as a MISS;
  `X-Cache: HIT|MISS`. A corrupt/old cached value decode-fails → treated as a MISS.
- `stream:true` → full bypass (no key, no Get/Set). Bodies over `GATEWAY_CACHE_MAX_BODY_BYTES`
  (default 1 MiB) → bypass with the **full** body preserved (`io.LimitReader` + `io.MultiReader`,
  no truncation). Redis Get/Set errors → log + serve live (never a client error).

Real-Redis behavior is integration-tested via testcontainers in CARD-011.
