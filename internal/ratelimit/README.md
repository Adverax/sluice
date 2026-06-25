# internal/ratelimit

Per-API-key rate limiting (COMP-008/009, FR-004) — a two-tier limiter and the
distributed-limit ACL (ADR-0010).

## Tiers

1. **Local token bucket** — `Registry` over `golang.org/x/time/rate`, one bucket per key.
   Fast path, no network. Rate/burst from `GATEWAY_RATELIMIT_RPS` / `_BURST`.
2. **Distributed cap** — `RateLimitRepository` port enforcing one global limit across
   gateway instances. `redisrepo` (go-redis/v9) implements it with an atomic Lua
   `INCR + PEXPIRE + PTTL` fixed window; `memrepo` is an in-memory equivalent (single
   instance / tests). A request must pass **both** tiers.

## Bounded registry (no unbounded growth)

The local `Registry` is size-capped (`GATEWAY_RATELIMIT_MAX_KEYS`, default 100000) with
LRU eviction, plus a periodic sweep that drops **full (idle) buckets** — safe because a
full bucket means the key isn't being throttled. `Close()` stops the sweep goroutine
(deferred in `cmd/gateway`). This prevents the memory-exhaustion vector that ephemeral
keys would otherwise create.

> Known v1 trade-off: under cap pressure, LRU can evict a paused-but-throttled key,
> resetting its **local** limit — the **global** Redis cap still holds. The per-source-IP
> ephemeral-key issuance cap (backlog IDEA-002) is the planned hardening.

## Fail-open

On a repository (Redis) error the middleware logs WARN and falls back to the local
limiter rather than rejecting — a Redis blip can't 503/429 the whole fleet. (A
degradation metric lands with observability, CARD-009.)

Real-Redis behavior is integration-tested via testcontainers in CARD-011.
