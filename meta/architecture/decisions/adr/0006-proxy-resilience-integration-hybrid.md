# ADR-0006: Integration of CTX-001 (Proxy) and CTX-002 (Resilience) — Hybrid Approach

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

CTX-001 (Proxy) must check the rate limit and circuit breaker state before sending a request to the provider (FR-004, FR-007). CTX-002 (Resilience) manages `RateLimitState` and `CircuitBreakerState`. Within a single Go process (CON-003) two fundamentally different integration mechanisms are possible.

First challenge: rate limiting requires the API key, which is available from the `Authorization` header — before the request body is parsed. Second challenge: the circuit breaker operates per-provider, and the provider is determined from the `model` field in the JSON request body — only after parsing. Constraint CON-001 requires idiomatic Go (stdlib-first); NFR-001 requires overhead ≤ 20ms on the hot path.

## Decision

We adopt the `hybrid` approach: rate limiting is implemented as a `net/http` middleware (the API key is available from the header before the body is parsed); the per-provider circuit breaker is placed directly at the call site inside the Proxy core (after the provider is resolved from the `model` field). This is an idiomatic stdlib-first approach (CON-001): a middleware chain for cross-cutting concerns, and a direct call via interface for provider-specific logic.

**Exact request-processing order (composition order):**

1. **rate-limit middleware** (`net/http` middleware) — first in the chain: reads the API key from the `Authorization` header, checks the limit. On exceeded limit — immediate return of `429 Too Many Requests` **before** any further work.
2. **resolve provider** — the specific provider is determined from the `model` field in the JSON request body.
3. **retry wraps breaker**: `retry( breaker.Execute(providerCall) )` — the retry loop wraps the call through the circuit breaker.

**Critical rule for retry and breaker interaction:**

If the breaker is in the `open` state, `breaker.Execute` returns `gobreaker.ErrOpenState` **immediately** without invoking the provider. The retry loop **must not** retry against an open breaker — `ErrOpenState` is a signal to switch to a fallback provider (routing, see DEC-009/ADR-0009), **not** a signal to retry the same failing provider.

This composition prevents two failure classes:
- **(a) "retries finish off an already-failing provider"** — retry does not hammer a provider that has already exceeded its error threshold.
- **(b) "retries spin uselessly into an open breaker"** — retry does not enter a spin-loop burning retry budget against an immediate `ErrOpenState`.

**Honest note on v1 scope:** v1 uses a single (mock) provider, so there is no fallback target. On `ErrOpenState` or exhausted retry budget, v1 **fast-fails** to the client with `503 Service Unavailable` + `Retry-After` header. Routing to a fallback provider on `ErrOpenState` is a documented extension for production (added when multiple providers are available), not a v1 feature. This is made explicit so that the composition is correct by design from the start.

## Alternatives considered

### direct_interface_call (fully direct calls)

Proxy calls both `RateLimiter` and `CircuitBreaker` via Go interfaces directly in the request handler — dependency injection at startup. Explicit dependencies, easy to mock. Rejected as the primary approach: rate limiting does not require knowledge of the request body and naturally belongs in the middleware chain, which is consistent with `net/http` idioms.

### middleware_chain (fully middleware)

Both rate limiting and circuit breaking are implemented as HTTP middleware. Clean separation: the Proxy handler contains no resilience logic. Rejected for the circuit breaker: a generic middleware has no access to the `model` field from the request body without additional parsing or passing it through the request context. This complicates the middleware and breaks the single-responsibility principle.

## Consequences

### Positive
- Rate limiting in middleware is idiomatic for `net/http`: middleware executes before the handler, the API key is available from the header without additional body parsing. Consistent with CON-001.
- Circuit breaker at the call site provides direct access to the resolved provider — no need to pass the provider name through context or headers.
- Clear separation of responsibilities: the middleware layer for header-based concerns, direct calls for body-based concerns.

### Negative
- The "hybrid" approach requires understanding two integration patterns, which slightly increases the onboarding cost for new developers.
- The boundary between CTX-001 and CTX-002 is "soft": Proxy core knows about the `CircuitBreaker` interface directly — this is acceptable within CON-003 (single service), but violates strict context isolation.
- The rule "do not retry into an open breaker" must be implemented explicitly: the retry loop must inspect the error type and not retry on `gobreaker.ErrOpenState`. If this check is omitted, the breaker degrades to a simple latency counter.

### Neutral
- The middleware chain is built at service initialization; the middleware order is fixed and documented in the Decision section above (rate limit → resolve provider → retry( breaker.Execute(providerCall) )).
- Testing of the rate-limit middleware and the circuit breaker call site is performed independently — each is tested with its own mock.
- OTel middleware (DEC-008) is inserted into the same chain; tracing order must cover both mechanisms (NFR-007).

## References

- DEC-006 (resolved by this ADR)
- CTX-001 (Proxy — hot path, handler)
- CTX-002 (Resilience — RateLimiter, CircuitBreaker)
- FR-004 (rate limiting, middleware)
- FR-007 (circuit breaker, per-provider)
- NFR-001 (overhead ≤ 20ms)
- CON-001 (stdlib-first, idiomatic Go)
- CON-003 (single service)

## History

- 2026-06-25: Created — rate limiting as net/http middleware, circuit breaker at call site in Proxy core; hybrid approach balances idiomatic style and access to provider context.
