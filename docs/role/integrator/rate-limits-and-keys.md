# Rate limits and keys

The gateway enforces per-key rate limiting. Every request is associated with a
key, and each key has its own request quota. Exceeding the quota returns HTTP 429.

## Identifying yourself with an API key

Include your key in the `Authorization` header:

```
Authorization: Bearer <your-key>
```

You can also send a raw token without the `Bearer` prefix:

```
Authorization: <your-key>
```

In v1, keys are used **only** for rate limiting and usage tracking. They are not
validated against any store — any non-empty string works as a key.

## Ephemeral keys for keyless callers

If you send a request **without** an `Authorization` header, the gateway
automatically mints a cryptographically random ephemeral key for you and returns
it in two places:

- Response header: `X-Sluice-Api-Key: eph_<32 hex chars>`
- Cookie: `sluice_api_key=eph_<32 hex chars>` (HttpOnly, SameSite=Lax)

Your client must send this key back on every subsequent request to stay within
its own rate-limit bucket. You can do this in two ways:

1. **Header** — read `X-Sluice-Api-Key` from the first response and send it as
   `Authorization: <value>` on all subsequent requests.
2. **Cookie** — if your HTTP client follows cookies automatically (e.g. a browser
   or a cookie-jar-enabled client), the `sluice_api_key` cookie is sent back
   automatically.

If you do not send the key back, every request is treated as a new anonymous
caller and you receive a fresh key and a fresh (empty) bucket each time. This
effectively bypasses per-key rate limiting, so **always reuse the key you were
given**.

> Note: even a rate-limited (429) response includes the `X-Sluice-Api-Key`
> header if a key was minted for that request, so you can capture the key even
> when your first request is rejected.

## Rate limit exceeded: HTTP 429

When your key exceeds its quota you receive:

```
HTTP/1.1 429 Too Many Requests
Retry-After: <seconds>
Content-Type: application/json

{"error":"rate_limited","message":"rate limit exceeded; retry later"}
```

The `Retry-After` header tells you the minimum number of seconds to wait before
retrying. Always honour it.

### What to do on a 429

1. Read the `Retry-After` header value (in seconds).
2. Wait at least that long before sending another request with the same key.
3. Apply exponential backoff with jitter if you retry in a tight loop.

## Distributed rate limiting

When multiple gateway instances share the same Redis, the rate limit is enforced
globally across all instances for a given key. A keyless request minted an
ephemeral key on instance A will be counted on the same shared bucket if it
later hits instance B — provided the client sends the key back.

If Redis is unavailable, the gateway falls back to per-instance local limiting
and continues serving requests rather than returning errors.
