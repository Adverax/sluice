# sluice — Kanban Gantt

```mermaid
gantt
    title sluice implementation waves
    dateFormat X
    axisFormat Day %j

    section Wave 1
    CARD-001 Service bootstrap & lifecycle :done, p1, card1, 0, 2d

    section Wave 2
    CARD-002 Provider interface & mock     :done, p1, card2, after card1, 1.5d

    section Wave 3
    CARD-012 OpenAPI contract & codegen    :active, p1, card12, after card2, 1d

    section Wave 4
    CARD-003 Non-streaming proxy, router, health & timeouts :p1, card3, after card12, 2.5d

    section Wave 5
    CARD-004 SSE streaming & context cancellation          :p1, card4, after card3, 2d
    CARD-005 Per-key rate limiting + ephemeral key         :p1, card5, after card3, 2.5d
    CARD-006 Response cache (Redis, TTL + override)        :p2, card6, after card3, 1.5d
    CARD-007 Retries & circuit breaker                     :p1, card7, after card3, 2.5d
    CARD-008 Bounded worker pool & backpressure            :p1, card8, after card3, 2d
    CARD-009 Observability: metrics, tracing, panic        :p1, card9, after card3, 2.5d
    CARD-010 Async usage metering → Postgres               :p2, card10, after card3, 2d

    section Wave 6
    CARD-011 Load test, race-suite, CI & make up           :p2, card11, after card7 card8 card9 card10, 2.5d
```

## Wave plan

| Wave | Cards | Parallelism | Gate |
|------|-------|-------------|------|
| 1 | CARD-001 | sequential | — |
| 2 | CARD-002 | sequential | CARD-001 done |
| 3 | CARD-012 (OpenAPI contract & codegen, ADR-0011) | sequential | CARD-002 done |
| 4 | CARD-003 | sequential | CARD-002 + CARD-012 done |
| 5 | CARD-004, CARD-005, CARD-006, CARD-007, CARD-008, CARD-009, CARD-010 | fully parallel | CARD-003 done (CARD-010 also after CARD-001) |
| 6 | CARD-011 | sequential | CARD-007, CARD-008, CARD-009, CARD-010 all done |

## Total estimate

~24d (sequential critical path: 2 + 1.5 + 1 + 2.5 + 2.5 + 2.5 = 12d; Wave 5 cards run in parallel)
