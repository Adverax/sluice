# Metering (CTX-004) — how it works

**Group:** integrations · **Bounded context:** CTX-004 Metering · **Capability:** CAP-006 Usage metering (FR-014)

Asynchronous per-request usage accounting: every completed inference produces one
`UsageEvent` that is written to Postgres **off the hot path**, through a bounded buffer and
a single background worker. The proxy's only coupling to metering is a one-way, non-blocking
enqueue; when the buffer is full, events are dropped (and counted) rather than blocking the
request (INV-003 / INV-007, ADR-0007).

## Topics (L4)

| # | File | What it covers |
|---|------|----------------|
| 01 | [01-usage-metering.md](01-usage-metering.md) | CAP-006 end to end: why async (INV-003), the one-way enqueue, the bounded `Buffer` + drop-on-full, the batch+timer `Worker`, the pgx `MeteringRepository` ACL, the `metering_buffer_size` gauge, and the graceful-shutdown final flush. |

Diagrams (referenced inline from topic 01):
- [diagrams/01-usage-metering-01.puml](diagrams/01-usage-metering-01.puml) — async flow: proxy → buffer → worker → Postgres, with the drop-on-full branch.
- [diagrams/01-usage-metering-02.puml](diagrams/01-usage-metering-02.puml) — worker flush-decision loop + shutdown drain sequence.

## doc → code map

Derived from CTX-004 components in `meta/architecture/trace.yml` (COMP-016/017/018, all
mapped to `internal/metering/**.go`) plus the producer and wiring call sites.

| Real file | Role |
|-----------|------|
| `internal/metering/metering.go` | Package doc + ports: `UsageEvent`, `Sink`/`NopSink`, `DropRecorder`/`NopDropRecorder`, `BufferSizeRecorder`/`NopBufferSizeRecorder`, `MeteringRepository`. |
| `internal/metering/buffer.go` | **COMP-016 Usage Buffer** — bounded channel; `NewBuffer`, non-blocking `Enqueue` (drop-on-full + counter), `Events()`, `Len()`. |
| `internal/metering/worker.go` | **COMP-017 Metering Worker** — single goroutine; `run` loop (batch/timer flush, POL-004), `flush` (bounded retry, AC-037), `drain`, `Close`, `FlushedOnShutdown`, `publishBufferSize` (metering_buffer_size). |
| `internal/metering/pgxrepo.go` | **COMP-018 MeteringRepository** — pgx/v5 ACL (ADR-0010); narrow `Execer`, batch `INSERT` into `usage_events`. |
| `internal/server/server.go` | Producer (CTX-001): `recordUsage` builds a `UsageEvent` and calls `Sink.Enqueue` after each completed unary/stream inference (the one-way coupling). |
| `cmd/gateway/main.go` | Wiring: constructs `Buffer`/`PgxRepository`/`Worker`, injects metrics recorders, starts the worker, registers `Worker.Close` as an `OnShutdown` hook after the HTTP drain, logs `FlushedOnShutdown`. |
| `migrations/0001_usage_events.sql` | Target table `usage_events` (mirrors `UsageEvent`). |

## Related docs

- **ADRs:**
  - [ADR-0005 — Metering UsageBuffer capacity = 1000](../../../../meta/architecture/decisions/adr/0005-metering-buffer-capacity-1000.md)
  - [ADR-0007 — Proxy→Metering via buffered channel, drop-on-full](../../../../meta/architecture/decisions/adr/0007-proxy-metering-buffered-channel.md)
  - [ADR-0010 — ACL via per-context repository interface](../../../../meta/architecture/decisions/adr/0010-repository-interface-per-context.md)
- **Other aspects:**
  - [../../01-surface-api/proxy/](../../01-surface-api/proxy/) — the proxy hot path that performs the one-way `Enqueue` (the producer side of this coupling).
  - [../../03-operations/observability/](../../03-operations/observability/) — where `metering_buffer_size` (gauge) and `metering_events_dropped_total` (counter) are registered and exported.
- **Role docs:**
  - [docs/role/operator/](../../../role/operator/) — operating the gateway, including watching the metering metrics on shutdown/overload.
- **Package README:** [internal/metering/README.md](../../../../internal/metering/README.md) (terse developer view).
