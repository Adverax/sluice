# Caching

The gateway caches successful non-streaming responses in Redis. Subsequent
identical requests are served from the cache without contacting the provider.

## Cache hit and miss indicator

Every response to `POST /v1/chat/completions` includes an `X-Cache` header:

| Value | Meaning |
|-------|---------|
| `HIT` | Response was served from cache. The provider was not called. |
| `MISS` | Response was fetched live from the provider and stored in cache. |

## How the cache key is computed

The cache key is a SHA-256 hash of:
- HTTP method (`POST`)
- Path (`/v1/chat/completions`)
- Raw request body bytes

Two requests with the same JSON fields but different whitespace or key ordering
are treated as **different** cache entries. Submit identical byte-for-byte request
bodies to guarantee a cache hit.

Cache entries are **not** scoped per API key — identical request bodies share
the same cached response across all callers. The completion content is not
user-specific, so this is correct behaviour.

## Per-request TTL override

To control how long the response for a specific request remains cached, send
the `X-Cache-TTL` header with an integer value in **whole seconds**:

```sh
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Cache-TTL: 300' \
  -d '{"model":"mock","messages":[{"role":"user","content":"Hello!"}]}'
```

The value must be a positive integer. Invalid or non-positive values are ignored
and the server's configured default TTL applies instead.

> Only the **first** request that populates a cache entry sets that entry's TTL.
> Subsequent requests that hit the same key do not change its expiry time.

## Streaming requests are never cached

If your request includes `"stream": true`, the gateway bypasses the cache
entirely. No cache key is computed and no response is stored. The `X-Cache`
header is not present on streaming responses.

## Redis unavailability

If Redis is unavailable, the gateway skips the cache and proxies the request
directly to the provider. Cache errors are never surfaced to you as HTTP errors —
you always get a live response.
