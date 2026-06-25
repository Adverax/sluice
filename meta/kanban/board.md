# sluice — Kanban Board

> 12 cards v1 (all done) + hardening wave (CARD-013/014/015 from the requirements audit)
> Gantt: [meta/kanban/gantt.md](gantt.md)

---

## Ready (0)

_none_

---

## In Progress (1)

| CARD | Title | Phase | Pri | Est | Branch |
|------|-------|-------|-----|-----|--------|
| CARD-013 | HTTP provider adapter + pooled client | implementation | P1 | 2d | card/013-http-provider-pooled-client |

## Hardening wave (audit gaps) — ready

| CARD | Title | Gap | Pri |
|------|-------|-----|-----|
| CARD-014 | Streaming through resilience (breaker/pool + metrics/span) | #3 | P1 |
| CARD-015 | Conformance: Redis token-bucket, metering_buffer_size metric, drained/flushed log | #5 | P2 |

---

## Review (0)

_none_

---

## Done (12)

| CARD | Title | Pri | Score | Actual | Merge |
|------|-------|-----|-------|--------|-------|
| CARD-001 | Service bootstrap & lifecycle | P1 | 9.0 | 0.1d | 9638bf7 |
| CARD-002 | Provider interface & mock | P1 | 9.0 | 0.1d | e2f2af9 |
| CARD-012 | OpenAPI contract & codegen scaffolding | P1 | 9.5 | 0.1d | 4fa2d54 |
| CARD-003 | Non-streaming proxy, router, health & timeouts | P1 | 9.0 | 0.1d | 9593466 |
| CARD-007 | Retries & circuit breaker | P1 | 9.0 | 0.1d | e6a98db |
| CARD-008 | Bounded worker pool & backpressure | P1 | 9.0 | 0.1d | 18ee178 |
| CARD-005 | Per-key rate limiting (local→Redis) + ephemeral key | P1 | 8.5 | 0.1d | 2c406a2 |
| CARD-009 | Observability: metrics, tracing, panic recovery | P1 | 9.5 | 0.1d | 90cb4b6 |
| CARD-004 | SSE streaming & context cancellation | P1 | 9.0 | 0.1d | 9f09517 |
| CARD-006 | Response cache (Redis, TTL + per-request override) | P2 | 9.0 | 0.1d | 2a25abf |
| CARD-010 | Async usage metering → Postgres | P2 | 9.5 | 0.1d | 86f2f21 |
| CARD-011 | Load test, race-suite, CI & make up | P2 | 9.5 | 0.1d | b0e400b |

---

## Skipped (0)

_none_

---

## Summary

- **Total cards:** 12 · **all merged** · 0 escalated
- **Avg review score:** 9.13 (range 8.5–9.5)
- **Review cycles:** 5 of 12 cards needed a fix cycle (severity gate caught real defects)
- **Features:** 10 · **Enablers:** 2 (CARD-011, CARD-012)
- **main:** all packages green under `go test -race`; integration suite (testcontainers) live-green
