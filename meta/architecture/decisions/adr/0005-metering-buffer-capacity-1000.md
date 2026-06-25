# ADR-0005: Metering UsageBuffer Capacity — 1000 Events

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

CTX-004 (Metering) accumulates usage records in an in-memory buffer (`UsageBuffer`) and flushes them in batches to Postgres via a background worker (FR-014). Invariant INV-003 prohibits blocking the hot path — when the buffer overflows, events are dropped without waiting. Invariant INV-007 allows event loss on overflow. CON-006 fixes asynchronous writes as a hard constraint.

Without a concrete buffer capacity value, acceptance criterion AC-036 (`TestMetering_BufferFull_DropsWithoutBlocking`) is not executable: the test does not know how many events need to be generated to trigger an overflow. NFR-003 requires no goroutine leaks; NFR-003 and memory constraints dictate a reasonable upper bound on the buffer.

## Decision

We adopt the `buffer_1000` strategy: the capacity of the `UsageBuffer` buffered channel is 1000 events. On overflow an event is dropped (non-blocking send via `select` / `default`) and the `metering_events_dropped_total` counter is incremented. A capacity of 1000 provides a small memory footprint with a fixed and well-testable overflow threshold.

## Alternatives considered

### buffer_10000

UsageBuffer capacity = 10000 events. A larger buffer reduces drop frequency during load spikes. Rejected for two reasons: the overflow test (`AC-036`) would require generating 10001 events, making it slow and resource-intensive; additionally, the increased memory footprint is not justified for a reference project where event loss is already accepted in INV-007.

## Consequences

### Positive
- AC-036 becomes executable: the test generates 1001 events and verifies that the N+1-th is dropped without blocking — a concrete, fast test.
- Small memory footprint: 1000 pointers to `UsageRecord` structs occupy only a few megabytes.
- A fixed threshold makes overflow behaviour predictable and operationally observable via the `metering_events_dropped_total` metric.

### Negative
- Under high load with a slow Postgres (or temporary unavailability) the buffer may overflow and some usage events will be lost. This is consistent with INV-007 (drops are acceptable), but the operator must monitor `metering_events_dropped_total`.
- The value of 1000 is not configurable in v1 — changing the threshold will require a rebuild.

### Neutral
- The `metering_events_dropped_total` counter (Prometheus) must be registered in the shared registry (DEC-008) for overflow monitoring.
- The Metering background worker must be fast enough that the buffer does not overflow under normal load. A benchmark test is required.
- On graceful shutdown (AC-032), Proxy closes the channel after draining in-flight requests; Metering must drain the remaining buffer before exiting.

## References

- DEC-005 (resolved by this ADR)
- CTX-004 (Metering — UsageBuffer, background worker)
- CTX-001 (Proxy — writes to UsageBuffer)
- FR-014 (asynchronous usage event writes, AC-035, AC-036, AC-037)
- NFR-003 (no goroutine leaks)
- INV-003 (hot path is not blocked), INV-007 (drops are acceptable)
- CON-006 (writes only asynchronously)

## History

- 2026-06-25: Created — UsageBuffer capacity = 1000 events, overflow drop without blocking + counter; makes AC-036 an executable test.
