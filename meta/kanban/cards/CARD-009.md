# CARD-009: Observability — metrics, tracing, panic recovery

**Status:** ready
**Priority:** P1
**Category:** feature
**Estimate:** 2.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/009-observability-metrics-tracing-panic
**Worktree:** —
**Source:** meta/architecture/handoff.md#increment-4
**Depends on:** CARD-003
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Implement the full observability layer under `internal/metrics/**.go`, `internal/tracing/**.go`, and `internal/middleware/**.go`.

**Metrics Registry & Exporter (COMP-013):**
- ADR-0008: use a shared, **injected** `*prometheus.Registry` (not `prometheus.DefaultRegisterer`); create metrics via `promauto.With(reg)`.
- Expose `GET /metrics` via `promhttp.HandlerFor(reg, promhttp.HandlerOpts{})`.
- Register all **6 required metrics** (NFR-007 / AC-048): `http_requests_total`, `http_request_duration_seconds`, `gateway_inflight_requests`, `provider_request_duration_seconds`, `ratelimit_rejected_total`, `breaker_state`.
- Wire metric increments at the appropriate middleware points (e.g., inflight gauge around proxy call, duration histogram in the outermost handler).

**OTel Tracing Middleware (COMP-014):**
- `internal/tracing/**.go` — initialize OTel SDK with OTLP/gRPC or OTLP/HTTP exporter (endpoint from env).
- `internal/middleware/**.go` — HTTP middleware creates a root span for each incoming request; nested span for the upstream provider call.
- **Collector-down tolerance:** exporter errors must be silently logged; they must **never** interrupt request processing (AC-050).

**Panic Recovery Middleware (COMP-007):**
- `internal/middleware/**.go` — `recover()` in a deferred function; on panic: return 500 to the client, log at ERROR with `panic_value` field and stack trace, then continue serving subsequent requests (AC-033, AC-034).
- Process must survive a panic in the handler goroutine (AC-033).
- If a sub-goroutine panics, the process must not terminate; the next request must receive a valid response (AC-034).

ADR-0008: all observability is injected; no package-level globals.

## Acceptance criteria

### FR-010 — Prometheus metrics

**AC-029**
- **Given:** several requests to gateway have been executed
- **When:** Prometheus scraper sends GET /metrics
- **Then:** response contains metrics `http_requests_total`, `http_request_duration_seconds`, `gateway_inflight_requests`, `ratelimit_rejected_total`, `breaker_state` in Prometheus text format
- **Test:** `TestMetrics_ExposesRequiredMetrics` (kind: happy)

### NFR-007 — All 6 metrics present

**AC-048**
- **Given:** gateway is running with Prometheus enabled
- **When:** GET /metrics is requested
- **Then:** response contains `http_requests_total`, `http_request_duration_seconds`, `gateway_inflight_requests`, `provider_request_duration_seconds`, `ratelimit_rejected_total`, `breaker_state`
- **Test:** `TestMetrics_AllSixMetricsPresent` (kind: happy)

### FR-011 — OTel tracing

**AC-030**
- **Given:** OTel collector is available, gateway is configured with an OTel exporter
- **When:** API Client sends a request
- **Then:** OTel collector receives a trace with at least two spans: incoming HTTP and upstream call
- **Test:** `TestTracing_EndToEndSpanCreated` (kind: happy)

**AC-050**
- **Given:** OTel collector is unavailable (network unreachable)
- **When:** API Client sends a request
- **Then:** request is processed successfully (200); trace export error is logged but does not interrupt processing
- **Test:** `TestTracing_CollectorDown_DoesNotBreakRequest` (kind: error)

### FR-013 — Panic recovery

**AC-033**
- **Given:** handler calls `panic("test panic")`
- **When:** an HTTP request arrives at that handler
- **Then:** process continues running, client receives 500, panic value and stack trace are recorded in logs
- **Test:** `TestPanicRecovery_Returns500AndContinues` (kind: happy)

**AC-034**
- **Given:** panic occurs in a goroutine of the handler that is separate from the main stack
- **When:** panic occurs
- **Then:** process does not terminate; the next incoming HTTP request receives a 200 or 500 response without hanging
- **Test:** `TestPanicRecovery_SubgoroutinePanicHandled` (kind: negative)

### FR-016 — Panic logged at ERROR

**AC-041**
- **Given:** panic in a handler
- **When:** panic recovery fires
- **Then:** log record contains ERROR level and the `panic_value` field
- **Test:** `TestLogging_PanicLoggedAtError` (kind: negative)

## Architecture context

- **FR:** FR-010, FR-011, FR-013, FR-016 (panic log AC)
- **NFR:** NFR-007
- **ADR:** ADR-0008
- **Components:** COMP-013 Metrics Registry & Exporter, COMP-014 OTel Tracing Middleware, COMP-007 Panic Recovery Middleware
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
