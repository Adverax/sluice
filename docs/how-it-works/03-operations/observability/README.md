# Observability (CTX-003)

> **Group:** operations · **Bounded context:** CTX-003 Observability · **Capability:** CAP-004 — Observability and health.

How `sluice` instruments itself for the operator and the orchestrator: liveness and
readiness probes, Prometheus metrics, end-to-end OpenTelemetry traces, and structured
per-request logs. This context owns no business aggregates — it *observes* the state of
the Proxy (CTX-001), Resilience (CTX-002), and Metering (CTX-004) contexts, which
register their metrics into a single shared, injected `*prometheus.Registry`
(conformist relationship, [ADR-0008](../../../../meta/architecture/decisions/adr/0008-observability-shared-prometheus-registry.md)).
Externally it is an open host service to Prometheus (EXT-004) and a conformist to the
OpenTelemetry collector (EXT-005).

The four components: COMP-012 Health & Readiness Handlers, COMP-013 Metrics Registry &
Exporter, COMP-014 OTel Tracing Middleware, COMP-015 Structured Logger. See the C4
diagram: [`c4/components-observability.puml`](../../../../meta/architecture/c4/components-observability.puml).

## Topics

| File | Covers |
|---|---|
| [01-health-and-readiness.md](01-health-and-readiness.md) | `/healthz` liveness (always 200) vs `/readyz` readiness (200/503), the `Checker` port, concurrent per-check timeouts, Redis + Postgres checkers, orchestrator probe wiring |
| [02-metrics.md](02-metrics.md) | Injected-registry design (ADR-0008), the eight registered metrics with names/labels/types, where each is incremented, route-label cardinality control, `/metrics` exposition via `promhttp.HandlerFor` |
| [03-tracing-and-logging.md](03-tracing-and-logging.md) | OTLP/HTTP batch export + collector-down tolerance (no-op fallback), the two-span (root + provider) trace, structured slog per request (request-id, latency, status), panic logging |
| [diagrams/](diagrams/) | `01-instrumentation-flow.puml` (request → middleware chain → registry/exporter/logger), `02-readiness-check.puml` (readiness sequence) |

## Doc → code map

Generated from the CTX-003 components in
[`meta/architecture/trace.yml`](../../../../meta/architecture/trace.yml).

| File | Component | Role |
|---|---|---|
| [`internal/health/health.go`](../../../../internal/health/health.go) | COMP-012 | `Checker` port, `Handler`, concurrent `Evaluate`, liveness/readiness verdict |
| [`internal/health/checkers.go`](../../../../internal/health/checkers.go) | COMP-012 | Redis + Postgres readiness checkers over narrow pinger ports |
| [`internal/metrics/metrics.go`](../../../../internal/metrics/metrics.go) | COMP-013 | Injected registry, `promauto.With(reg)` registration of the metric set, `Recorder` port, `NopRecorder` |
| [`internal/metrics/middleware.go`](../../../../internal/metrics/middleware.go) | COMP-013 | HTTP metrics middleware (inflight, latency, count), route-label cardinality control |
| [`internal/metrics/infer.go`](../../../../internal/metrics/infer.go) | COMP-013 | `InstrumentInferFunc` / `InstrumentStreamFunc` — provider-latency histogram on the call seam |
| [`internal/tracing/tracing.go`](../../../../internal/tracing/tracing.go) | COMP-014 | OTLP/HTTP exporter, batch processor, collector-down tolerance, no-op fallback, `Provider` + `Shutdown` |
| [`internal/tracing/infer.go`](../../../../internal/tracing/infer.go) | COMP-014 | Child provider spans (`provider.infer` / `provider.stream`) on the call seam |
| [`internal/middleware/tracing.go`](../../../../internal/middleware/tracing.go) | COMP-014 | HTTP root-span middleware, low-cardinality span naming |
| [`internal/logging/logging.go`](../../../../internal/logging/logging.go) | COMP-015 | `slog.Logger` construction (json/text, level), injected via DI |
| [`internal/logging/middleware.go`](../../../../internal/logging/middleware.go) | COMP-015 | Per-request log, request-id generation/propagation, `LogPanic` |
| [`internal/middleware/recover.go`](../../../../internal/middleware/recover.go) | COMP-014 / CAP-005 | Outermost panic recovery (reuses `LogPanic`), 500 response, `SafeGo` |
| [`internal/server/server.go`](../../../../internal/server/server.go) | COMP-012 / COMP-013 | `GetHealthz`, `GetReadyz`, `GetMetrics` HTTP seams serving the above |
| [`cmd/gateway/main.go`](../../../../cmd/gateway/main.go) | wiring | Constructs the injected registry, tracer, logger, health checkers; composes the middleware chain |

## Related docs

- **ADR:** [ADR-0008 — Observability via Shared Prometheus Registry](../../../../meta/architecture/decisions/adr/0008-observability-shared-prometheus-registry.md)
- **Operator role docs:** [`docs/role/operator/monitoring-and-metrics.md`](../../../role/operator/monitoring-and-metrics.md), [`docs/role/operator/health-and-readiness.md`](../../../role/operator/health-and-readiness.md)
- **Aspects that emit the metrics observed here:**
  - [`../../01-surface-api/proxy/`](../../01-surface-api/proxy/) — HTTP request + inflight metrics, traced/logged request path
  - [`../../02-resilience/resilience/`](../../02-resilience/resilience/) — `ratelimit_rejected_total`, `breaker_state`, panic recovery
  - [`../../04-integrations/metering/`](../../04-integrations/metering/) — `metering_events_dropped_total`, `metering_buffer_size`
