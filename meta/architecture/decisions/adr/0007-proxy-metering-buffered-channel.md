# ADR-0007: Passing Usage Events from CTX-001 (Proxy) to CTX-004 (Metering) via Buffered Channel

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

CTX-001 (Proxy) must deliver usage events (provider, model, tokens, latency, status) to CTX-004 (Metering) after each request completes. CON-006 prohibits synchronous writes to Postgres on the hot path. INV-003 requires that the hot path never blocks on writing usage data. INV-007 allows event loss on buffer overflow (DEC-005 fixed the capacity at 1000 events).

Within a single Go process (CON-003) the event-passing mechanism between contexts must conform to Go idioms (CON-001). Key questions: what happens on buffer overflow, and how does Metering receive the shutdown signal during graceful shutdown. NFR-003 (no goroutine leaks) constrains the acceptable approaches.

## Decision

We adopt the `buffered_channel_drop_on_full` strategy: Proxy delivers events to CTX-004 via a Go buffered channel with a non-blocking send (`select` / `default`). On overflow the event is dropped and the `metering_events_dropped_total` counter is incremented. A Metering background worker reads from the channel. On graceful shutdown Proxy closes the channel after draining in-flight requests; Metering drains the remaining events and flushes them to Postgres (AC-032).

## Alternatives considered

### fire_and_forget_goroutine

For each usage event Proxy launches a separate goroutine `go func() { metering.Record(...) }`. Simple implementation, no risk of blocking. Rejected because it leads to unbounded goroutine growth under load — a direct violation of NFR-003 (0 leaked goroutines) and NFR-006 (bounded goroutines). There is no backpressure: when Postgres is unavailable, memory grows without bound. Graceful shutdown does not guarantee that all launched goroutines will complete.

## Consequences

### Positive
- The hot path never blocks (INV-003 satisfied): non-blocking send via `select` / `default` returns control immediately.
- Simple Go idiom with no external dependencies: a buffered channel and a single background worker (CON-001, CON-002).
- Explicit and operationally observable overflow behaviour: the `metering_events_dropped_total` counter in Prometheus (DEC-008).
- Graceful shutdown with flush guarantee: AC-032 (`TestGracefulShutdown_FlushesMetering`) is verifiable.

### Negative
- Event loss on buffer overflow is acceptable per INV-007, but the operator must monitor `metering_events_dropped_total` — a non-zero value indicates that tuning is needed (increase buffer capacity or flush throughput).
- Graceful shutdown requires coordinated shutdown ordering: Proxy must close the channel only after all in-flight requests complete, and Metering must drain the channel to completion before exiting. The shutdown sequence order must be strictly defined.
- **Billing durability: an honest limitation of the approach.** Usage events are a **billing ledger**. Drop-on-full means record loss under load. In-memory drop is **acceptable for a PoC/reference repository**, but **is not billing-grade durability**. Production would require a durable queue or write-ahead log (e.g., Kafka or a local WAL) before writing to Postgres. This is a deliberate maturity gap: "what I would add for production".

### Neutral
- The buffer capacity of 1000 events is fixed by DEC-005; this decision depends on it.
- A single Metering background worker is created at service initialization and exits on receiving the shutdown signal. Its lifecycle is managed via context cancellation.
- When Postgres is unavailable (AC-037) the background worker logs the error; the retry strategy or batch-drop strategy is a separate implementation question within CTX-004.
- **Background flusher sizing.** The Metering background worker flushes events using a dual trigger: **batch size OR timer** (flush when a batch is assembled OR when the timer fires). Sizes are chosen so that drops occur **only under genuine overload**, not on normal traffic peaks. The 1000-event buffer (DEC-005) is calibrated to the expected burst; the flusher must sustain throughput such that in steady-state and on normal peaks the channel never fills.

## References

- DEC-007 (resolved by this ADR)
- CTX-001 (Proxy — event sender)
- CTX-004 (Metering — receiver, background worker)
- FR-014 (asynchronous usage writes, AC-035, AC-036, AC-037)
- FR-012 (graceful shutdown, AC-032)
- NFR-003 (no goroutine leaks)
- INV-003 (hot path is not blocked), INV-007 (drops are acceptable)
- CON-006 (writes only asynchronously)

## History

- 2026-06-25: Created — buffered channel with non-blocking send and drop-on-full; Metering background worker; graceful shutdown with flush via close-and-drain.
