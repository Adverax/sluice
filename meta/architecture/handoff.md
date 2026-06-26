# sluice — Implementation Handoff

## 1. Header

**sluice** is a thin, high-throughput LLM Gateway in Go (1.23+, stdlib-first): it proxies `POST /v1/chat/completions` to the correct upstream provider (routed by the `model` field), returning plain JSON or SSE streams. It adds a resilience layer (per-API-key token-bucket rate limiting local→Redis, bounded worker-pool backpressure, retries with backoff+jitter, per-provider circuit breaker), a Redis response cache, full observability (Prometheus, OpenTelemetry, structured slog, `/healthz` + `/readyz`), and asynchronous usage metering to Postgres that never blocks the hot path. It is one service (not microservices) with an idiomatic `internal/<area>` layout, brought up end-to-end via `make up` (gateway + postgres + redis + prometheus + grafana).

**Status:** 16 FR / 8 NFR / 6 CON / 10 ADRs / 4 contexts / 18 components — validators green.

---

## 2. Architecture at a glance

Four logical bounded contexts inside a single process (CON-003); boundaries reflect language/lifecycle/rate-of-change, not deployment units.

| Context | Key components |
|---|---|
| **CTX-001 Proxy** (hot path; CAP-001, CAP-005) | COMP-001 HTTP Handler & Router, COMP-002 Proxy Core, COMP-003 Retry Engine, COMP-004 Cache Adapter, COMP-005 Provider Interface & Mock, COMP-006 Lifecycle Manager, COMP-007 Panic Recovery Middleware |
| **CTX-002 Resilience** (CAP-002, CAP-003) | COMP-008 Rate Limit Middleware, COMP-009 RateLimitRepository, COMP-010 Worker Pool / Backpressure, COMP-011 Circuit Breaker |
| **CTX-003 Observability** (CAP-004) | COMP-012 Health & Readiness Handlers, COMP-013 Metrics Registry & Exporter, COMP-014 OTel Tracing Middleware, COMP-015 Structured Logger |
| **CTX-004 Metering** (CAP-006) | COMP-016 Usage Buffer, COMP-017 Metering Worker, COMP-018 MeteringRepository |

### Cross-context wiring (governing ADRs)

- **Proxy → Resilience** — sync hot-path call, customer/supplier; Proxy checks rate-limit + breaker before upstream. **ADR-0006** (hybrid composition order).
- **Proxy → Metering** — one-way buffered Go channel after request completes; Proxy knows only the buffer, never Postgres; hot path never blocks (INV-003). **ADR-0007** (buffered_channel_drop_on_full).
- **Observability → Proxy / Resilience / Metering** — conformist, via a shared in-process Prometheus registry (injected). **ADR-0008** (shared_prometheus_registry).
- **Proxy → LlmProvider (EXT-001)** — anti-corruption via the single `Provider` Go interface. **ADR-0009**.
- **Proxy/Resilience → Redis (EXT-002), Metering → Postgres (EXT-003)** — repository interface per context. **ADR-0010**.
- Rate-limit anonymous-key handling: **ADR-0001** (ephemeral_assigned_key). Breaker thresholds: **ADR-0002** (volume_based_50pct). Pool size: **ADR-0003** (env_configurable). Cache TTL: **ADR-0004** (default_5min_per_request_override). Buffer cap: **ADR-0005** (buffer_1000).

### C4 diagrams

- `meta/architecture/c4/context.puml`
- `meta/architecture/c4/containers.puml`
- `meta/architecture/c4/components-proxy.puml`
- `meta/architecture/c4/components-resilience.puml`
- `meta/architecture/c4/components-observability.puml`
- `meta/architecture/c4/components-metering.puml`

---

## 3. Implementation increments

Four increments, each a working state, aligned with the 4 contexts and the spec §12 layered build order.

### Increment 1 — Skeleton & happy-path proxy (spec layers 1–2)

**Goal:** Bootable service with config/DI, lifecycle, liveness/readiness, structured logging, and a working non-streaming proxy to a mock provider with timeouts at every boundary.

**Delivers:** FR-001 (non-stream path), FR-002, FR-008, FR-009, FR-016, NFR-004 (timeout coverage), NFR-005 (graceful drain), and partial FR-012 (shutdown skeleton).

**Components → target paths:**
- COMP-001 HTTP Handler & Router — `internal/proxy/**.go`, `internal/server/**.go`
- COMP-002 Proxy Core — `internal/proxy/**.go`
- COMP-005 Provider Interface & Mock — `internal/provider/**.go`
- COMP-006 Lifecycle Manager — `internal/lifecycle/**.go`, `cmd/gateway/**.go`
- COMP-012 Health & Readiness Handlers — `internal/health/**.go`
- COMP-015 Structured Logger — `internal/logging/**.go`

**Planned tests:**
- `TestProxy_HappyPath_NonStreaming`, `TestProxy_ProviderError_Returns502`, `TestProxy_InvalidBody_Returns400`
- `TestRouter_RoutesToCorrectProvider`, `TestRouter_MissingModel_Returns400`, `TestRouter_UnknownModel_Returns404`
- `TestHealthz_ReturnsOK`
- `TestReadyz_AllDepsUp_Returns200`, `TestReadyz_RedisDown_Returns503`, `TestReadyz_PostgresDown_Returns503`
- `TestLogging_StructuredFieldsPresent`
- `TestConfig_AllBoundariesHaveTimeouts`
- `TestGracefulShutdown_DrainsInFlightRequests`, `TestGracefulShutdown_ZeroDropped`

**Governing ADRs:** ADR-0006, ADR-0009, ADR-0008, ADR-0010.

---

### Increment 2 — Streaming, rate limiting & cache (spec layers 3–4)

**Goal:** SSE streaming with cancellation, per-key token-bucket rate limiting (local→Redis with ephemeral-key minting) returning 429 + Retry-After, and a TTL-based response cache with per-request override; repository ACLs over Redis.

**Delivers:** FR-001 (streaming AC), FR-003 (context cancellation), FR-004, FR-005.

**Components → target paths:**
- COMP-002 Proxy Core (`InferStream`) — `internal/proxy/**.go`
- COMP-005 Provider Interface & Mock — `internal/provider/**.go`
- COMP-004 Cache Adapter (CacheRepository) — `internal/cache/**.go`
- COMP-008 Rate Limit Middleware — `internal/ratelimit/**.go`, `internal/middleware/**.go`
- COMP-009 RateLimitRepository — `internal/ratelimit/**.go`

**Planned tests:**
- `TestProxy_HappyPath_Streaming`
- `TestProxy_ClientCancel_AbortsUpstream`, `TestProxy_StreamingClientCancel_AbortsUpstream`
- `TestRateLimit_ExceedLimit_Returns429WithRetryAfter`, `TestRateLimit_WithinLimit_Passes`, `TestRateLimit_MissingApiKey_HandledGracefully`, `TestRateLimit_DistributedRedis_GlobalLimit`
- `TestCache_Hit_ReturnsCachedResponse`, `TestCache_Miss_FetchesAndCaches`, `TestCache_StreamingNotCached`, `TestCache_RedisDown_FallsThrough`

**Governing ADRs:** ADR-0001 (ephemeral key), ADR-0004 (cache TTL + override), ADR-0010 (repository ACL), ADR-0006, ADR-0009.

---

### Increment 3 — Resilience & bounded concurrency (spec layers 5–6)

**Goal:** Retries (exponential backoff + jitter, deadline-aware, no-retry-on-ErrOpenState/client errors), circuit breaker (gobreaker tumbling 10s window / ≥10 req / ≥50% failures, open Timeout 60s), bounded worker pool (`GATEWAY_WORKER_POOL_SIZE`, default 100) with backpressure 503 + Retry-After, composed per ADR-0006.

**Delivers:** FR-006, FR-007, FR-015, NFR-002 (overload graceful degradation), NFR-003 (no goroutine leaks), NFR-006 (upstream goroutines bounded).

**Components → target paths:**
- COMP-003 Retry Engine — `internal/proxy/retry/**.go`
- COMP-011 Circuit Breaker — `internal/breaker/**.go`
- COMP-010 Worker Pool / Backpressure — `internal/pool/**.go`

**Planned tests:**
- `TestRetry_TransientError_SucceedsOnThirdAttempt`, `TestRetry_ExhaustedAttempts_Returns502`, `TestRetry_ContextDeadlineExpired_NoRetry`, `TestRetry_ClientError_NoRetry`
- `TestCircuitBreaker_OpenState_FastFail`, `TestCircuitBreaker_ThresholdExceeded_Opens`, `TestCircuitBreaker_HalfOpen_SuccessClosesCircuit`
- `TestWorkerPool_Saturated_Returns503WithRetryAfter`, `TestWorkerPool_RecoveryAfterSaturation`
- `BenchGateway_Overload3x_NocrashGracefulDegradation`, `BenchGateway_GoroutineLeakCheck`, `BenchGateway_GoroutineCountBounded`

**Governing ADRs:** ADR-0002 (volume-based 50%), ADR-0003 (env-configurable pool), ADR-0006 (composition order).

---

### Increment 4 — Observability, metering, load test & polish (spec layers 7–10)

**Goal:** Prometheus metrics via injected registry, OTel tracing (collector-down tolerated), Grafana dashboard, async usage metering (buffered channel, drop-on-full, batch flush to Postgres, buffer 1000), panic-recovery middleware, table-driven `-race` tests with testcontainers, k6 load scenario + RESULTS.md + pprof goroutine-leak check, README.

**Delivers:** FR-010, FR-011, FR-012 (final metering-flush AC), FR-013, FR-014, NFR-001 (p95 overhead ≤ 20ms), NFR-007 (6/6 metrics), NFR-008 (race-free suite).

**Components → target paths:**
- COMP-013 Metrics Registry & Exporter — `internal/metrics/**.go`
- COMP-014 OTel Tracing Middleware — `internal/tracing/**.go`, `internal/middleware/**.go`
- COMP-007 Panic Recovery Middleware — `internal/middleware/**.go`
- COMP-016 Usage Buffer — `internal/metering/**.go`
- COMP-017 Metering Worker — `internal/metering/**.go`
- COMP-018 MeteringRepository — `internal/metering/**.go`

**Planned tests:**
- `TestMetrics_ExposesRequiredMetrics`, `TestMetrics_AllSixMetricsPresent`
- `TestTracing_EndToEndSpanCreated`, `TestTracing_CollectorDown_DoesNotBreakRequest`
- `TestPanicRecovery_Returns500AndContinues`, `TestPanicRecovery_SubgoroutinePanicHandled`, `TestLogging_PanicLoggedAtError`
- `TestMetering_AsyncFlush_PersistsRecords`, `TestMetering_BufferFull_DropsWithoutBlocking`, `TestMetering_PostgresDown_NoHotpathBlock`
- `TestGracefulShutdown_FlushesMetering`, `TestGracefulShutdown_TimeoutForced`
- `BenchGateway_p95OverheadUnder20ms`
- `TestSuite_RaceFree`

**Governing ADRs:** ADR-0008 (shared registry), ADR-0007 (buffered channel drop-on-full), ADR-0005 (buffer 1000), ADR-0010.

---

## 4. Definition of done per increment

- **Increment 1:** All listed ACs green under `go test -race`; service boots via `cmd/gateway`, `/healthz` + `/readyz` respond, every boundary has a >0 timeout (AC-045), SIGTERM drains in-flight with exit code 0.
- **Increment 2:** All listed ACs green under `-race`; SSE forwarded with flush and cancelled on client disconnect; distributed rate limit holds across two instances on shared Redis (AC-013); cache falls through when Redis is down.
- **Increment 3:** All listed ACs green under `-race`; **NFR-002** `BenchGateway_Overload3x_NocrashGracefulDegradation` (0 crashes, only 200/429/503, recovers), **NFR-003** `BenchGateway_GoroutineLeakCheck` (baseline ±5), **NFR-006** `BenchGateway_GoroutineCountBounded` (≤ pool limit) all pass.
- **Increment 4:** All listed ACs green under `-race`; **NFR-001** `BenchGateway_p95OverheadUnder20ms` met over a 5-minute plateau; **NFR-007** all 6 metrics present; **NFR-008** full suite passes `go test -race ./...` with 0 data races; k6 RESULTS.md captured; CI (build + test -race + golangci-lint, CON-004) green; `make up` brings up the full stack (CON-005).

---

## 5. Open follow-ups (non-blocking)

- **Fallback-provider routing on `ErrOpenState`** (ADR-0002 / ADR-0006) — multi-provider failover when a breaker is open; currently a single provider per model fast-fails. Documented future extension.
- **Per-IP ephemeral-key issuance cap** (ADR-0001) — anti-bypass guard so anonymous clients cannot mint unbounded ephemeral keys to evade per-key limits. Future hardening.
- **Durable queue / WAL for billing-grade metering** (ADR-0007) — current design drops on buffer-full and on flush failure (best-effort, not billing-grade). A durable WAL/queue would close the gap if metering becomes revenue-critical.
- **FR-003 negative-AC nit** — FR-003 (client-cancel) carries only `happy`-kind ACs (AC-008, AC-009); consider adding a negative/boundary AC (e.g. cancel after upstream already responded) for completeness.

---

## 6. Increment — 2026-06-26 — OpenAI compatibility (Variant B; ADR-0012, ADR-0013)

**Goal:** make sluice a real, drop-in **OpenAI-compatible** LLM gateway in front of a real model — **Ollama** as the primary showcase backend (OpenAI / vLLM / LM Studio via config). Both the UPSTREAM adapter and the gateway's own EDGE speak the real OpenAI `/v1/chat/completions` wire format (ports & adapters: OpenAI edge adapter ↔ canonical core ↔ OpenAI upstream adapter). Canonical core + resilience + metering + observability are **unchanged** — the OpenAI shape lives only in the edge and upstream adapters; only canonical `provider.Request`/`Response`/`Chunk` cross the Provider boundary (ADR-0009 preserved).

New requirements: **FR-017..FR-021**; new constraints **CON-007** (endpoint scope) / **CON-008** (non-goals). New config: `GATEWAY_UPSTREAM_URL` (exists; Ollama `http://localhost:11434/v1`), `GATEWAY_UPSTREAM_API_KEY` (optional), `GATEWAY_UPSTREAM_MODEL` (default doc `llama3.2`).

Two independently shippable cards. **CARD-016 ships first** (upstream adapter; edge stays simplified — a valid intermediate state). **CARD-017** then flips the edge to the real OpenAI shape.

### CARD-016 — OpenAI-compatible UPSTREAM adapter

**Scope:** A real HTTP provider adapter (behind the existing `Provider` port, ADR-0009) that speaks the real OpenAI wire to the configured backend.
- Canonical `Request` → OpenAI `/v1/chat/completions` request (`model`, `messages[{role,content}]`, `stream`, `temperature`, `top_p`, `max_tokens`, `stop`).
- Parse the real OpenAI response: unary `choices[0].message.content` + `usage` → canonical `Response`; streaming `choices[0].delta` + `[DONE]` → canonical `Chunk`s.
- Streaming metering: set `stream_options.include_usage=true`, parse the trailing usage chunk into terminal `Chunk.Usage`; **graceful 0/uncounted** if the upstream omits usage (no error, no stream break).
- Upstream auth: send `Authorization: Bearer <GATEWAY_UPSTREAM_API_KEY>` only when the key is non-empty; **omit when empty** (Ollama).
- Config: `GATEWAY_UPSTREAM_URL` / `GATEWAY_UPSTREAM_API_KEY` / `GATEWAY_UPSTREAM_MODEL`; **register the adapter for the configured model**; model passed through as-is.
- **Update the in-process `MockUpstream`** (`mockupstream.go`) to emit the **real OpenAI shape** (unary + SSE `chat.completion.chunk` + `[DONE]` + usage chunk) so unit + load tests exercise the real mapping. **Keep** the interface-level mock `Provider`. Mock stays the default `make up` upstream (fast demo).
- **Edge stays as-is** (valid intermediate state).

**Covers:** FR-021, FR-019 (upstream/streaming-usage half) · ADR-0013, ADR-0009, ADR-0010 · COMP-005 (Provider Interface & Adapter) · `internal/provider/**.go`, `cmd/gateway/**.go` (config + registration).
**ACs:** AC-062 (unary mapping), AC-063 (optional bearer auth), AC-064 (mock upstream emits OpenAI shape), AC-059 (streaming missing-usage graceful 0).
**Tests:** `TestUpstream_OpenAIAdapter_UnaryMapping`, `TestUpstream_OpenAIAdapter_BearerAuthOptional`, `TestUpstream_MockUpstream_EmitsOpenAIShape`, `TestEdge_Streaming_MissingUsage_GracefulZero`.

### CARD-017 — OpenAI-compatible EDGE

**Scope:** Rework the public edge to the real OpenAI request/response/stream/error shape (ADR-0012, under ADR-0011 contract-first discipline).
- Rework `api/openapi.yaml` to the real OpenAI request/response/stream/error schema with **liberal-accept** (`additionalProperties: true`); modeled subset (`model`, `messages`, `stream`, `temperature`, `top_p`, `max_tokens`, `stop`) forwarded; unknown fields (`seed`, `user`, penalties, `logit_bias`, `response_format`, `n`, `logprobs`) accepted-but-ignored — never 400.
- `go generate` → regenerate `internal/api/`; update server DTO mapping (OpenAI DTO ↔ canonical `provider.Request`/`Response`/`Chunk`).
- Unary response: real `chat.completion` shape with **edge-generated** `id` (`chatcmpl-…`) / `created` / `object`; single `choices[0]`; `system_fingerprint` omitted.
- Streaming: emit `chat.completion.chunk` SSE events + literal `data: [DONE]`.
- Errors: OpenAI envelope `{error:{message,type,code}}` for 400/401/429/502/503 and mapped upstream errors.
- Genuine drop-in for OpenAI SDKs; update the integrator API reference (`docs/role/integrator/**`) + README `curl` examples to the OpenAI shape.
- Add an **OPTIONAL** Ollama docker-compose profile + documented `GATEWAY_UPSTREAM_URL=…:11434/v1` usage; keep the **mock as the default `make up` upstream** so the demo stays fast.

**Covers:** FR-017, FR-018, FR-020, FR-019 (edge/SSE half) · ADR-0012, ADR-0011, ADR-0009 · COMP-001 (HTTP Handler & Router), COMP-002 (Proxy Core) · `api/openapi.yaml`, `internal/api/**.go`, `internal/server/**.go`, `internal/proxy/**.go`, `docs/role/integrator/**`, `README.md`, `docker-compose.yml` (optional Ollama profile).
**ACs:** AC-053 (accepted), AC-054 (unknown ignored not 400), AC-055 (unsupported content 400), AC-056 (unary OpenAI shape), AC-057 (edge-generated fields), AC-058 (streaming chunks + [DONE]), AC-060 (gateway error shape), AC-061 (upstream error mapped).
**Tests:** `TestEdge_OpenAIRequest_Accepted`, `TestEdge_UnknownFields_IgnoredNot400`, `TestEdge_UnsupportedContent_Returns400`, `TestEdge_UnaryResponse_OpenAIShape`, `TestEdge_UnaryResponse_EdgeGeneratedFields`, `TestEdge_Streaming_OpenAIChunksAndDone`, `TestEdge_GatewayError_OpenAIShape`, `TestEdge_UpstreamError_MappedToOpenAIShape`.

**Definition of done:** all listed ACs green under `go test -race`; an unmodified OpenAI SDK / `curl` example completes a unary and a streaming chat against sluice (mock upstream by default, Ollama via the optional profile); `make up` still boots fast on the mock; CI (build + test -race + golangci-lint, CON-004) green.
