# ADR-0001: Ephemeral Assigned Key for Anonymous Requests

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

The gateway implements per-API-key rate limiting (FR-004). A key is identified via the `Authorization` header and serves as the basis for bucket isolation. A non-goal of the project is the absence of auth validation: the key is an identifier only — its authenticity is not verified.

The problem arises when the `Authorization` header is missing: the system must decide how to apply rate limiting to such requests. If all anonymous requests share a single bucket, the noisy-neighbor effect occurs — one client can exhaust the limit for all other anonymous clients. Constraint CON-001 requires a stdlib-first approach without heavy frameworks.

Base context: CTX-002 (Resilience) governs rate limiting; CTX-001 (Proxy) handles incoming requests and reads headers. Within a single Go process (CON-003) the decision requires no network calls for key generation.

## Decision

We adopt the `ephemeral_assigned_key` strategy: when the `Authorization` header is absent, the gateway generates a cryptographically random key, attaches a fresh per-key rate-limit bucket to it, and returns the key to the client via the `X-Sluice-Api-Key` response header (and via `Set-Cookie` for browser clients). The client carries this key for the session and receives an isolated bucket. The key is **generated**, not validated against any store — this preserves the non-goal of "no auth validation".

## Alternatives considered

### anonymous_key (single anonymous bucket)

All requests without `Authorization` are served from a single shared bucket with metadata `"anonymous"`. Compatible with the non-goal of "no auth validation" and simple to implement. Rejected due to the noisy-neighbor effect: one aggressive client exhausts the limit for all anonymous users, making rate limiting ineffective for load isolation.

### reject_401

Return `401 Unauthorized` when the `Authorization` header is absent. Explicit and safe behaviour. Rejected because it directly contradicts the non-goal of "no auth validation" — the boundary becomes blurred and the gateway de facto requires authentication, which is not its purpose.

### pass_through (no rate limit for anonymous requests)

Requests without a key are passed through without rate limiting. Maximally compatible with the non-goal. Rejected because it renders rate limiting completely useless for unauthenticated clients, opening the door to unlimited abuse.

## Consequences

### Positive
- Every anonymous client receives its own isolated rate-limit bucket — the noisy-neighbor effect is eliminated.
- The non-goal of "no auth validation" is preserved: the key is generated cryptographically at random without touching any store.
- Browser clients receive the key automatically via `Set-Cookie`, simplifying integration without additional client-side logic.

### Negative
- A client that does not persist the issued key (or deliberately ignores it) will receive a new bucket on every request — this allows circumventing the rate limit. Mitigation: limit the number of keys issued per source IP (follow-up, not implemented in v1 — scope discipline).
- The number of active rate-limit buckets can grow without bound if clients do not reuse keys. A TTL on buckets is required (follow-up).

### Neutral
- The `X-Sluice-Api-Key` header is added to the response — clients expecting a minimal response may find this unexpected.
- A mechanism for generating cryptographically random keys (`crypto/rand`) is required; it is already available in the stdlib.
- AC-012 (`TestRateLimit_MissingApiKey_HandledGracefully`) covers this behaviour and must verify that the key is issued in the response header.

## References

- DEC-001 (resolved by this ADR)
- CTX-001 (Proxy — reads/sets request/response headers)
- CTX-002 (Resilience — manages rate-limit buckets)
- FR-004 (rate limiting per-API-key)
- CON-001 (stdlib-first, no auth frameworks)

## History

- 2026-06-25: Created — gateway generates an ephemeral key for anonymous requests, providing per-client isolation without violating the non-goal of "no auth validation".
