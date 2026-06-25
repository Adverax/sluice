# sluice — Kanban Board

> 11 cards · ~23d total · 5 waves
> Gantt: [meta/kanban/gantt.md](gantt.md)

---

## Ready (11)

| CARD | Title | Status | Pri | Cat | Est | Depends | Skill |
|------|-------|--------|-----|-----|-----|---------|-------|
| CARD-001 | Service bootstrap & lifecycle | ready | P1 | feature | 2d | — | golang-pro |
| CARD-002 | Provider interface & mock | ready | P1 | feature | 1.5d | CARD-001 | golang-pro |
| CARD-003 | Non-streaming proxy, router, health & timeouts | ready | P1 | feature | 2.5d | CARD-002 | golang-pro |
| CARD-004 | SSE streaming & context cancellation | ready | P1 | feature | 2d | CARD-003 | golang-pro |
| CARD-005 | Per-key rate limiting (local→Redis) + ephemeral key | ready | P1 | feature | 2.5d | CARD-003 | golang-pro |
| CARD-006 | Response cache (Redis, TTL + per-request override) | ready | P2 | feature | 1.5d | CARD-003 | golang-pro |
| CARD-007 | Retries & circuit breaker | ready | P1 | feature | 2.5d | CARD-003 | golang-pro |
| CARD-008 | Bounded worker pool & backpressure | ready | P1 | feature | 2d | CARD-003 | golang-pro |
| CARD-009 | Observability: metrics, tracing, panic recovery | ready | P1 | feature | 2.5d | CARD-003 | golang-pro |
| CARD-010 | Async usage metering → Postgres | ready | P2 | feature | 2d | CARD-003, CARD-001 | golang-pro |
| CARD-011 | Load test, race-suite, CI & make up | ready | P2 | enabler | 2.5d | CARD-007, CARD-008, CARD-009, CARD-010 | golang-pro |

---

## In Progress (0)

_none_

---

## Review (0)

_none_

---

## Done (0)

_none_

---

## Skipped (0)

_none_

---

## Gantt

See [meta/kanban/gantt.md](gantt.md) for the full Mermaid dependency chart with wave breakdown.

---

## Summary

- **Total cards:** 11
- **Total estimate:** ~23d
- **Waves:** 5
- **P1 cards:** 8 (CARD-001 through CARD-009 minus CARD-006)
- **P2 cards:** 3 (CARD-006, CARD-010, CARD-011)
- **Features:** 10 · **Enablers:** 1
