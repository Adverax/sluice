# sluice — Kanban Board

> 12 cards · ~24d total · 6 waves
> Gantt: [meta/kanban/gantt.md](gantt.md)

---

## Ready (4)

| CARD | Title | Status | Pri | Cat | Est | Depends | Skill |
|------|-------|--------|-----|-----|-----|---------|-------|
| CARD-004 | SSE streaming & context cancellation | ready | P1 | feature | 2d | CARD-003 ✓ | golang-pro |
| CARD-006 | Response cache (Redis, TTL + per-request override) | ready | P2 | feature | 1.5d | CARD-003 | golang-pro |
| CARD-009 | Observability: metrics, tracing, panic recovery | ready | P1 | feature | 2.5d | CARD-003 ✓ | golang-pro |
| CARD-010 | Async usage metering → Postgres | ready | P2 | feature | 2d | CARD-003 ✓, CARD-001 ✓ | golang-pro |
| CARD-011 | Load test, race-suite, CI & make up | ready | P2 | enabler | 2.5d | CARD-007, CARD-008, CARD-009, CARD-010 | golang-pro |

---

## In Progress (1)

| CARD | Title | Phase | Pri | Est | Branch |
|------|-------|-------|-----|-----|--------|
| CARD-009 | Observability: metrics, tracing, panic recovery | implementation | P1 | 2.5d | card/009-observability |

---

## Review (0)

_none_

---

## Done (7)

| CARD | Title | Pri | Score | Actual | Merge |
|------|-------|-----|-------|--------|-------|
| CARD-001 | Service bootstrap & lifecycle | P1 | 9.0 | 0.1d | 9638bf7 |
| CARD-002 | Provider interface & mock | P1 | 9.0 | 0.1d | e2f2af9 |
| CARD-012 | OpenAPI contract & codegen scaffolding | P1 | 9.5 | 0.1d | 4fa2d54 |
| CARD-003 | Non-streaming proxy, router, health & timeouts | P1 | 9.0 | 0.1d | 9593466 |
| CARD-007 | Retries & circuit breaker | P1 | 9.0 | 0.1d | e6a98db |
| CARD-008 | Bounded worker pool & backpressure | P1 | 9.0 | 0.1d | 18ee178 |
| CARD-005 | Per-key rate limiting (local→Redis) + ephemeral key | P1 | 8.5 | 0.1d | 2c406a2 |

---

## Skipped (0)

_none_

---

## Gantt

See [meta/kanban/gantt.md](gantt.md) for the full Mermaid dependency chart with wave breakdown.

---

## Summary

- **Total cards:** 12
- **Total estimate:** ~24d
- **Waves:** 6 (CARD-012 OpenAPI scaffolding inserted before CARD-003 per ADR-0011)
- **P1 cards:** 9 · **P2 cards:** 3
- **Features:** 10 · **Enablers:** 2 (CARD-011, CARD-012)
