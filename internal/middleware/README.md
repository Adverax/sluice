# internal/middleware

`net/http` middleware for the gateway's request pipeline. Each middleware is a
`func(http.Handler) http.Handler` composed in `cmd/gateway` (outermost first):

```
logging → rate-limit → counting → generated routes (+ OpenAPI validation)
```

## RateLimiter (COMP-008, CARD-005)

`ratelimit.go` — per-API-key rate limiting (FR-004, ADR-0001/0006). Outermost of the
request *work*, so a 429 short-circuits before the proxy/pool/provider are touched (INV-004).

- **Key resolution** (precedence): `Authorization` header → a well-formed `sluice_api_key`
  cookie (`eph_` + 32 hex, validated) → mint a fresh `crypto/rand` key.
- **Ephemeral minting** (ADR-0001): keyless request → mint, return via `X-Sluice-Api-Key`
  header + HttpOnly `Set-Cookie`, apply a fresh bucket. The cookie round-trips so the next
  request reuses the same bucket (no new registry entry).
- **Fail-closed** on `crypto/rand` error → 500 (never issues a weak/guessable key).
- **Over limit** → 429 + `Retry-After`, next handler not called.

Backing limiter tiers and the distributed ACL live in `internal/ratelimit`.

> Later cards add to this package: OpenTelemetry tracing + panic-recovery middleware (CARD-009).
