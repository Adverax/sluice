# resilience — protecting the path and the upstreams

How sluice keeps itself and its providers healthy under load and failure: per-API-key
rate limiting, a bounded worker pool with backpressure, and a per-provider circuit
breaker — all deciding, before the upstream call, whether to let a request through and
how to fail fast when it shouldn't.

| Aspect | Topics |
|--------|--------|
| [Resilience](resilience/) | [01 · Rate limiting & backpressure](resilience/01-rate-limiting-and-backpressure.md) — token bucket (local + Redis), ephemeral keys, worker pool · [02 · Circuit breaking](resilience/02-circuit-breaking.md) — gobreaker state machine, fast-fail, composition order |

Bounded context: **CTX-002 Resilience** (CAP-002 Rate limiting & backpressure, CAP-003
Circuit breaking). It is called synchronously from the [proxy](../01-surface-api/) hot path
(customer/supplier), and its state is surfaced by [observability](../03-operations/)
(`breaker_state`, `ratelimit_rejected_total`).
