# CARD-009: Observability — metrics, tracing, panic recovery

**Status:** done
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
**Review score:** 9.5 (2 cycles; cycle-1 important: metric/span cardinality, fixed; 6 ACs ✓)
**Started:** 2026-06-25T10:29:02Z
**Closed:** 2026-06-25T11:03:54Z
**Actual:** 0.1d
**Merge commit:** 90cb4b6
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

Implemented the full observability layer on branch `card/009-observability`.

**Packages / files**
- `internal/metrics/` — `metrics.go` (injected `*prometheus.Registry` via
  `promauto.With(reg)`, ADR-0008; the six metrics; `Recorder` port +
  `NopRecorder`), `middleware.go` (HTTP `http_requests_total` +
  `http_request_duration_seconds` + `gateway_inflight_requests` gauge),
  `infer.go` (`InstrumentInferFunc` → `provider_request_duration_seconds`),
  `metrics_test.go`.
- `internal/tracing/` — `tracing.go` (OTel SDK init, OTLP/HTTP exporter, BATCH
  processor, no-op fallback, `NewWithProvider` test seam), `infer.go` (nested
  `provider.infer` span), `tracing_test.go`.
- `internal/middleware/` — `recover.go` (`Recoverer` + `SafeGo`), `tracing.go`
  (root-span HTTP middleware), `recover_test.go`; `ratelimit.go` gained an
  injected `rejectRecorder` port (`WithRejectRecorder`).
- `internal/server/server.go` — `WithMetricsRegistry`; `GetMetrics` now serves
  `promhttp.HandlerFor(reg, …)` via a `metricsResponse` adapter on the generated
  `GetMetricsResponseObject` seam (no change to `api.gen.go` / `openapi.yaml`).
- `cmd/gateway/main.go` — composition root: builds `prometheus.NewRegistry()`,
  `metrics.New`, `tracing.New` (endpoint from `GATEWAY_OTEL_ENDPOINT`), wires the
  chain `recover → logging → tracing → metrics → rate-limit → counting → routes`.

**Metric wiring**
- `http_requests_total{route,status}` + `http_request_duration_seconds{route}` +
  `gateway_inflight_requests`: outer metrics middleware (inflight inc/dec in a
  defer so a panic still decrements).
- `provider_request_duration_seconds{provider}`: `met.InstrumentInferFunc`
  wraps the composed pool→retry→breaker→provider InferFunc.
- `ratelimit_rejected_total`: incremented at the 429 reject path via the injected
  `rejectRecorder` (ratelimit never imports Prometheus — ADR-0008 boundary).
- `breaker_state{provider}`: set from `breaker.WithOnStateChange` (closed=0,
  half-open=1, open=2) via `met.SetBreakerState` — breaker stays Prometheus-free.

**Tracing collector-down strategy (AC-050)**: BATCH (async) span processor, so
exports run off the request path; exporter/resource init errors are logged at
WARN and downgrade to a no-op provider (never block boot or a request). Test
wires an always-erroring exporter through the batch processor and asserts 200 +
no hang.

**Panic recovery**: `Recoverer` (outermost) recovers handler-goroutine panics →
500 + `logging.LogPanic` (ERROR + `panic_value` + stack), process survives;
re-raises `http.ErrAbortHandler`. `SafeGo` wraps detached goroutines (a panic in
a detached goroutine can't be caught by the handler defer — Go semantics) with
its own recover. AC-041 (`TestLogging_PanicLoggedAtError`) left untouched; the
middleware reuses `logging.LogPanic`.

**Deps added**: `github.com/prometheus/client_golang`,
`go.opentelemetry.io/otel` + `/sdk` + `/exporters/otlp/otlptrace/otlptracehttp`.

**AC → test**
- AC-029 → `TestMetrics_ExposesRequiredMetrics`
- AC-048 → `TestMetrics_AllSixMetricsPresent`
- AC-030 → `TestTracing_EndToEndSpanCreated` (tracetest SpanRecorder, ≥2 spans,
  single trace)
- AC-050 → `TestTracing_CollectorDown_DoesNotBreakRequest`
- AC-033 → `TestPanicRecovery_Returns500AndContinues`
- AC-034 → `TestPanicRecovery_SubgoroutinePanicHandled`
- AC-041 → existing `TestLogging_PanicLoggedAtError` (kept green)

**Status**: `go build ./...` ✓, `go vet ./...` ✓, `go test -race ./...` ✓ (all
packages), `go generate ./...` diff-clean (api.gen.go / openapi.yaml untouched),
`go mod tidy` settled. Pre-existing `SA1019 api.GetSwagger` lint warning in
server.go is out of scope (unchanged code).
