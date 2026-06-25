# ADR-0004: Response Cache TTL — 5-Minute Default with Per-Request Override

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

The gateway caches responses to identical requests in Redis with a TTL (FR-005). CTX-001 (Proxy) performs cache lookups and writes; the store is EXT-002 (Redis), integrated via ACL (DEC-010). User story US-006 mentions "optional TTL per request", but no concrete default TTL value is defined in the inputs.

Without a numeric default TTL, acceptance criteria AC-014 (`TestCache_Hit_ReturnsCachedResponse`) and AC-015 (`TestCache_Miss_FetchesAndCaches`) cannot be written reproducibly — the test does not know when the entry will expire. Constraint CON-001 requires stdlib-first solutions.

## Decision

We adopt the `default_5min_per_request_override` strategy: the response cache TTL is 5 minutes by default. A client may override the TTL for a specific request by passing a value in the `X-Cache-TTL` header (in seconds). A value of 5 minutes is a reasonable balance between data freshness and upstream load for LLM requests.

## Alternatives considered

### fixed_5min (no per-request override)

TTL = 5 minutes, hard-coded with no ability to override at the request level. Simplicity and ease of testing. Rejected because it does not satisfy the US-006 requirement ("optional TTL per request") — different request types (short reference queries vs. long generative ones) have different freshness requirements.

## Consequences

### Positive
- AC-014 and AC-015 become executable: tests know the concrete TTL value and can reproducibly verify hit/miss behaviour.
- Flexibility for clients: ability to set `X-Cache-TTL: 0` to disable caching for a specific request, or to increase TTL for stable requests.
- Satisfies US-006 (optional per-request TTL).

### Negative
- Additional parsing of the `X-Cache-TTL` header on the hot path. A client can abuse this by setting `TTL=0` on every request, effectively disabling the cache. Validation is required: a minimum acceptable TTL or ignoring zero and negative values.
- Documenting the header semantics (units, boundary values) becomes part of the gateway's public API.

### Neutral
- The value of 5 minutes is used as a constant in tests and documentation; it may be made env-configurable in the future if needed (follow-up).
- Streaming responses (`"stream":true`) are not cached regardless of TTL (AC-016) — behaviour is unchanged.
- When Redis is unavailable the cache is bypassed without an error (AC-017) — TTL plays no role in this scenario.

## References

- DEC-004 (resolved by this ADR)
- CTX-001 (Proxy — cache lookup and write)
- FR-005 (response caching, AC-014, AC-015, AC-016, AC-017)
- US-006 (optional per-request TTL)

## History

- 2026-06-25: Created — cache TTL of 5 minutes by default with per-request override via `X-Cache-TTL` header; makes AC-014/AC-015 executable tests.
