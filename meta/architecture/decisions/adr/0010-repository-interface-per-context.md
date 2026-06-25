# ADR-0010: ACL via Per-Context Repository Interface for External Stores

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

CTX-002 (Resilience) and CTX-001 (Proxy) use EXT-002 (Redis) for distributed rate limiting and response caching respectively. CTX-004 (Metering) uses EXT-003 (Postgres) for persistent storage of usage records. The Anti-Corruption Layer (ACL) strategy must be defined: how to isolate domain logic from the details of specific client libraries (`go-redis/v9`, `pgx/v5`).

NFR-004 requires timeouts at all boundaries (Redis, Postgres). CON-001 fixes `go-redis/v9` and `pgx/v5` as permitted dependencies. CON-002 requires minimal dependencies. Without an ACL, domain code will directly depend on `redis.Client` and `pgx.Conn` types, making unit testing without real services impossible and complicating any potential client swap.

## Decision

We adopt the `repository_interface_per_context` strategy: each context defines its own Go repository interface — `CacheRepository` (CTX-001), `RateLimitRepository` (CTX-002), `MeteringRepository` (CTX-004). Interface implementations use `go-redis/v9` and `pgx/v5`. Domain code depends only on the interfaces; concrete implementations are injected at service initialization. Integration tests with real services use testcontainers.

## Alternatives considered

### direct_client_usage

Contexts use `go-redis/v9` and `pgx/v5` directly in business logic without an intermediate repository layer. Less code, faster start. Rejected for three reasons: it violates ACL (Redis/Postgres types leak into the domain); unit testing requires running real services or complex mocking of client libraries; swapping the client (e.g., `redis` → `valkey`, `pgx` → `stdlib/database/sql`) would require refactoring domain code.

## Consequences

### Positive
- Testability: repositories are mocked without Redis/Postgres in unit tests — fast and with no external dependencies.
- ACL is maintained: swapping the client library does not affect domain logic — only the repository implementation.
- Explicit dependency boundaries: domain packages do not import `go-redis` or `pgx` — statically verifiable.
- NFR-004 (timeouts): timeout configuration is encapsulated in the constructors of repository implementations.

### Negative
- More boilerplate: each store requires an interface + implementation + DI code. For a reference repository this is noticeable, but justified by testability requirements.
- Integration tests with real services require testcontainers — an additional dependency in the dev/CI environment.

### Neutral
- Three interfaces: `CacheRepository`, `RateLimitRepository`, `MeteringRepository` — reside in the domain packages of the respective contexts.
- Implementations reside in separate packages (infrastructure layer), not mixed with domain code.
- Connection configuration (DSN, timeouts) is managed via env variables and read at implementation initialization (CON-005 — `make up` via docker-compose).
- CI (CON-004) runs integration tests with testcontainers in a separate job or via a build tag.

## References

- DEC-010 (resolved by this ADR)
- CTX-001 (Proxy — CacheRepository → EXT-002)
- CTX-002 (Resilience — RateLimitRepository → EXT-002)
- CTX-004 (Metering — MeteringRepository → EXT-003)
- EXT-002 (Redis), EXT-003 (Postgres)
- FR-004 (rate limiting + Redis)
- FR-005 (caching + Redis)
- FR-014 (metering + Postgres)
- NFR-004 (timeouts at all boundaries)
- CON-001 (go-redis/v9, pgx/v5 permitted)
- CON-002 (minimal dependencies)

## History

- 2026-06-25: Created — per-context repository interface (CacheRepository, RateLimitRepository, MeteringRepository); implementations via go-redis/v9 and pgx/v5; testing via mocks and testcontainers.
