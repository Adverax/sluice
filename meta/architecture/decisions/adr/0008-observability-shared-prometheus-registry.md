# ADR-0008: Observability via Shared Prometheus Registry

**Status:** Accepted  
**Date:** 2026-06-25  
**Deciders:** @roman.miakotin  
**Revised:** —

## Context

CTX-003 (Observability) is a cross-cutting concern that owns no business aggregates. It must observe the state of CTX-001 (Proxy), CTX-002 (Resilience), and CTX-004 (Metering) within a single Go process (CON-003). Requirements include: Prometheus metrics export (FR-010), liveness/readiness probes (FR-008, FR-009), OTel traces (FR-011), and structured logs (FR-016).

NFR-007 requires 6 mandatory metrics at `/metrics`. Constraint CON-001 fixes `prometheus/client_golang` as an already-chosen dependency. The choice of integration mechanism affects module coupling and testability: when using a global registry, tests can interfere with each other due to shared state.

## Decision

We adopt the `shared_prometheus_registry` strategy: each context registers its metrics in a shared `prometheus.Registry` instance injected at service initialization (not the global `prometheus.DefaultRegisterer`, but an explicitly created `prometheus.NewRegistry()` — so that tests remain isolated). CTX-003 exposes the registry via `GET /metrics`. OTel traces are injected as middleware into CTX-001. Logs are passed via `slog.Logger` injected at initialization.

**Concrete API pattern:**

- Metric registration: `promauto.With(reg).NewCounterVec(...)` / `promauto.With(reg).NewHistogramVec(...)` etc. — always against the injected `*prometheus.Registry`, **never** against the global default registry.
- Endpoint export: `promhttp.HandlerFor(reg, promhttp.HandlerOpts{})`, **not** `promhttp.Handler()` (which uses the global default registry).
- In tests: each test creates `prometheus.NewRegistry()` — this is the exact mechanism that prevents `duplicate metrics registration` panics and metric leakage between tests. Test isolation is the **primary motivation** for choosing an injectable registry.

## Alternatives considered

### observability_callbacks (ObservabilityPort interface)

Each context accepts an `ObservabilityPort` (Recorder) interface and calls it on events. CTX-003 implements the interface via Prometheus + OTel. Explicit contract, easy to mock without global state, strict boundary. Rejected as over-engineering for a reference repository: an additional interface, implementation, and injection for each context, while `prometheus/client_golang` is already in the stack (CON-001). This is over-engineering that does not suit the scale of the project.

## Consequences

### Positive
- Standard Go practice for in-process metrics: `prometheus/client_golang` supports injecting a custom registry — no new dependencies (CON-001, CON-002).
- Minimal coupling: contexts register their metrics at initialization and do not know about CTX-003 directly.
- An explicit `prometheus.NewRegistry()` instead of the global `DefaultRegisterer` ensures test isolation: each test constructs a fresh `prometheus.NewRegistry()`, eliminating `duplicate metrics registration` panics and metric leakage between test cases. This is the key positive motivation for choosing an injectable registry (not convenience alone).

### Negative
- Without a strict convention, any package can register a metric in the shared registry — there is no compile-time check for metric namespace.
- When tests run in parallel, if they accidentally share a registry, registration conflicts (`duplicate metrics` panic) are possible. Test isolation must be maintained carefully.

### Neutral
- OTel middleware is inserted into the CTX-001 HTTP chain at initialization; exporter configuration (OTLP/HTTP endpoint, env GATEWAY_OTEL_ENDPOINT, default port 4318) is managed via env (EXT-005).
- `slog.Logger` is injected as a dependency into all contexts at startup — no global `log.Default()`.
- `GET /metrics`, `GET /healthz`, `GET /readyz` are registered in a separate HTTP mux of CTX-003, not in the main mux of CTX-001 — port or path separation is configured at startup.

## References

- DEC-008 (resolved by this ADR)
- CTX-001 (Proxy — OTel middleware, slog, in-flight metrics)
- CTX-002 (Resilience — breaker_state, ratelimit_rejected_total)
- CTX-003 (Observability — /metrics, /healthz, /readyz)
- CTX-004 (Metering — metering_events_dropped_total)
- FR-008, FR-009 (health probes)
- FR-010, FR-011 (metrics, tracing)
- FR-016 (structured logging)
- NFR-007 (6/6 mandatory metrics)
- CON-001 (prometheus/client_golang in stack)

## History

- 2026-06-25: Created — shared prometheus.Registry (not global default), OTel middleware in CTX-001, slog.Logger via DI; observability_callbacks pattern rejected as over-engineering for a reference repo.
