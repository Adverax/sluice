# ADR-0009: Single Provider Interface for LLM Provider Integration

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

CTX-001 (Proxy) must integrate with EXT-001 (LlmProvider) to execute inference requests. In v1 the provider is a controlled mock that allows the gateway to be tested without a real LLM service. Future support for real providers (OpenAI, Anthropic, and others) as new implementations is required.

The interface contract must be defined: at what level of abstraction it should reside, and how to handle provider-specific error formats (e.g., HTTP 429 from OpenAI and HTTP 429 from Anthropic with different response bodies). Without an Anti-Corruption Layer (ACL) the provider-specific protocol leaks into the Proxy domain, complicating the addition of a per-provider circuit breaker (FR-007). Constraint CON-001 requires idiomatic Go; CON-002 requires minimal dependencies.

## Decision

We adopt the `single_provider_interface` strategy: a single Go `Provider` interface is defined with two explicit methods. The mock implements the interface with configurable delay and error percentage for testing. Each real provider implementation maps provider-specific HTTP errors to domain errors within its own implementation (ACL). The per-provider circuit breaker (DEC-006) operates at the level of `Provider` method calls.

**Canonical interface contract:**

```go
type Provider interface {
    // Infer — unary path: one request → one response.
    Infer(ctx context.Context, req Request) (Response, error)

    // InferStream — streaming path (SSE, FR-002/US-002):
    // returns a chunk channel; stream initialisation error — in error.
    InferStream(ctx context.Context, req Request) (<-chan Chunk, error)
}
```

The alternative of a single method returning a streaming type (e.g., `Infer(...) (Stream, error)` with unification via a `Stream` interface) was considered and rejected: two explicit methods give a clearer separation of code paths and simplify independent mocking of each path.

**ACL contract on types:** `Request`, `Response`, `Chunk` are **canonical gateway types** (owned by CTX-001). Each provider adapter translates to/from the provider's wire format **and** normalises usage (tokens) into canonical fields. Consequence: CTX-004 (Metering) reads canonical usage from `Response`/`Chunk` — no provider-specific fields in metering code. This is the purpose of the ACL.

## Alternatives considered

### http_passthrough

Proxy forwards the HTTP request directly to the provider without a Go interface; routing is via per-model URL configuration. Less code, faster start. Rejected because it violates ACL: provider-specific HTTP protocol (authentication headers, error formats, streaming quirks) leaks into the Proxy domain. Adding a per-provider circuit breaker is impossible without coupling to a specific provider. Mocking for tests requires running an HTTP server, which complicates tests.

## Consequences

### Positive
- Maximum ACL isolation: the Proxy domain does not depend on LLM specifics — provider-specific errors, request formats, and authentication headers are encapsulated in the implementation.
- Testability: the mock provider implements `Provider` with configurable behaviour — no HTTP server needed in unit tests (CON-001).
- Extensibility via interface (TERM-003): adding a new provider = a new `Provider` implementation, with no changes to the Proxy domain.
- Per-provider circuit breaker (DEC-006) works natively: `gobreaker` wraps calls to `provider.Infer(...)` and `provider.InferStream(...)` of the specific implementation.
- **Canonical types as ACL guarantee:** CTX-004 (Metering) works only with `Response.Usage` / `Chunk.Usage` of the canonical type, importing no provider package. This is a verifiable compile-time context isolation invariant.

### Negative
- Each provider implementation must implement HTTP-to-domain error mapping — additional code (at minimum one `mapError` function per provider).
- Two interface methods (`Infer` + `InferStream`) mean that every mock and every implementation must cover both paths. For v1 with a single provider this is a minimal burden; as the number of providers grows it scales linearly.

### Neutral
- The v1 mock provider is configured via initialisation parameters (delay, error percentage) — the interface for test configuration is not part of `Provider`.
- When adding a real provider, integration documentation for error mapping is required.
- The `Provider` interface resides in the CTX-001 domain package; implementations reside in separate packages (one package per provider).

## References

- DEC-009 (resolved by this ADR)
- CTX-001 (Proxy — owns the Provider interface)
- EXT-001 (LlmProvider — external system)
- FR-001 (request proxying)
- FR-002 (routing by model)
- FR-007 (circuit breaker per-provider)
- CON-001 (idiomatic Go, testability)
- TERM-003 (extensibility via interface)

## History

- 2026-06-25: Created — single Provider interface with Infer method; ACL for error mapping in each implementation; mock for testing.
