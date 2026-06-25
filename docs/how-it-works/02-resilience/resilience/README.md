# Resilience (CTX-002) — how it works

Bounded context **CTX-002 Resilience** protects the hot path and the upstream
providers from overload and cascading failures. It owns two decisions made
*before* (and around) a provider call: **may this request proceed at all?**
(rate limiting + backpressure) and **is this provider healthy enough to call?**
(circuit breaking). Both live at the same level — they decide whether to let a
request through — and share the language of *limit*, *state*, and *fast fail*.

This aspect is a logical module boundary inside the single `sluice` Go process
(CON-003), not a separate service. It is consumed synchronously on the hot path
by CTX-001 (Proxy) — a `customer_supplier` relationship where Resilience defines
the contract (a `bool`/`error` verdict) and Proxy adapts to it — and it reaches
out to Redis (EXT-002) for the distributed rate-limit tier through an
anti-corruption layer (the `RateLimitRepository` port, ADR-0010).

## Files in this aspect

| File | What it explains |
|------|------------------|
| [01-rate-limiting-and-backpressure.md](01-rate-limiting-and-backpressure.md) | CAP-002 — per-API-key token-bucket rate limiting (local `x/time/rate` + distributed Redis Lua), ephemeral key lifecycle, fail-open, and the bounded worker pool / backpressure (semaphore, reject-before-work → 503, streaming slot lifecycle). |
| [02-circuit-breaking.md](02-circuit-breaking.md) | CAP-003 — `sony/gobreaker` state machine (closed → open → half-open), per-provider keying, trip thresholds, open-state fast-fail, why client cancellations are not counted, and the composition order. |
| [diagrams/](diagrams/) | PlantUML: rate-limit/backpressure flow, breaker state machine, composition seam. |

## doc → code map

The components below are CTX-002's entries in `meta/architecture/trace.yml`
(`components:` section). Each maps to the real source that implements it.

| Component | Real file(s) | Role |
|-----------|--------------|------|
| COMP-008 Rate Limit Middleware | [`internal/middleware/ratelimit.go`](../../../../internal/middleware/ratelimit.go) | net/http middleware: key resolution (Authorization / cookie / minted ephemeral), two-tier check (local → distributed), 429 + `Retry-After`, `ratelimit_rejected_total`. |
| COMP-008 (local tier) | [`internal/ratelimit/ratelimit.go`](../../../../internal/ratelimit/ratelimit.go) | `Registry` — per-key `x/time/rate` token-bucket registry, bounded by `MaxKeys`, with LRU eviction + idle sweep. |
| COMP-009 RateLimitRepository | [`internal/ratelimit/redisrepo.go`](../../../../internal/ratelimit/redisrepo.go) | `RedisRepository` — distributed token-bucket via an atomic Redis Lua script (ACL over `go-redis/v9`, ADR-0010). |
| COMP-009 (port + default adapter) | [`internal/ratelimit/memrepo.go`](../../../../internal/ratelimit/memrepo.go) | `RateLimitRepository` interface (the port) and `MemoryRepository` (in-process fixed-window default / test adapter). |
| COMP-010 Worker Pool / Backpressure | [`internal/pool/pool.go`](../../../../internal/pool/pool.go) | `Pool` — buffered-channel semaphore, non-blocking `tryAcquire`, `Guard` (unary) and `GuardStream` (stream, slot released exactly once). |
| COMP-011 Circuit Breaker | [`internal/breaker/breaker.go`](../../../../internal/breaker/breaker.go) | `Registry` — per-provider `gobreaker.CircuitBreaker`, `Execute`/`ExecuteStream`, `OnStateChange` → `breaker_state`. |
| Composition root | [`internal/proxy/resilience/resilience.go`](../../../../internal/proxy/resilience/resilience.go) | `Composer` — wires retry→breaker into `InferFunc`, breaker (no retry) into `StreamFunc`, maps to `*Unavailable` → 503. |
| Tuning | [`internal/config/config.go`](../../../../internal/config/config.go) | `config.Breaker` and `config.RateLimit` structs + defaults. |
| Wiring | [`cmd/gateway/main.go`](../../../../cmd/gateway/main.go) | Assembles pool → retry → breaker → provider and the middleware chain (ADR-0006). |

> Note: the **retry** engine (`internal/proxy/retry/`) belongs to CAP-001 / the
> Proxy aspect and its mechanism is documented there. This aspect only describes
> *where retry sits in the composition order* (between the pool and the breaker
> on the unary path; absent on the streaming path).

## Related docs

**ADRs**
- [ADR-0001 — Ephemeral assigned key for anonymous requests](../../../../meta/architecture/decisions/adr/0001-ephemeral-assigned-key-for-anonymous-requests.md)
- [ADR-0002 — Circuit breaker thresholds (volume-based, 50%)](../../../../meta/architecture/decisions/adr/0002-circuit-breaker-volume-based-thresholds.md)
- [ADR-0003 — Worker pool size is env-configurable](../../../../meta/architecture/decisions/adr/0003-worker-pool-env-configurable.md)
- [ADR-0006 — Proxy/Resilience integration (hybrid composition)](../../../../meta/architecture/decisions/adr/0006-proxy-resilience-integration-hybrid.md)
- [ADR-0010 — ACL via per-context repository interface](../../../../meta/architecture/decisions/adr/0010-repository-interface-per-context.md)

**Other aspects**
- [../../01-surface-api/proxy/](../../01-surface-api/proxy/) — the Proxy seam (`server.InferFunc`/`StreamFunc`) that Resilience wraps, and the retry engine.
- [../../03-operations/observability/](../../03-operations/observability/) — where `breaker_state` and `ratelimit_rejected_total` are exported.

**Operator role docs** (`docs/role/operator/`)
- [operating-under-load.md](../../../role/operator/operating-under-load.md) — load shedding, 429/503 behaviour, tuning knobs.
- [configuration-reference.md](../../../role/operator/configuration-reference.md) — `GATEWAY_RATELIMIT_*`, `GATEWAY_WORKER_POOL_SIZE`, `GATEWAY_BREAKER_*`.

## C4 reference

The synthesized component diagram for this context:
[`meta/architecture/c4/components-resilience.puml`](../../../../meta/architecture/c4/components-resilience.puml).
