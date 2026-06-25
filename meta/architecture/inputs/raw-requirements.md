# Raw Requirements — sluice

Intake queue. One line each; NFRs marked with a metric. Downstream
`forge:architect-domain-extraction` formalizes these into FR/NFR/CON.

## Functional (FR)

# processed: 2026-06-24 → FR-001
- Proxy `POST /v1/chat/completions` to a provider and return the response. Source: US-001.

# processed: 2026-06-24 → FR-001
- Support SSE streaming responses with `http.Flusher` flush when `"stream": true`. Source: US-002.

# processed: 2026-06-24 → FR-002
- Route to a provider based on the requested model field. Source: US-003.

# processed: 2026-06-24 → FR-003
- Propagate `context.Context` end-to-end; client cancellation aborts the upstream call. Source: US-004.

# processed: 2026-06-24 → FR-004
- Per-API-key rate limiting (token bucket, local `x/time/rate` + Redis distributed); on exceed return `429` with `Retry-After`. Source: US-005.

# processed: 2026-06-24 → FR-005
- Cache identical responses in Redis with a TTL (optional per request). Source: US-006.

# processed: 2026-06-24 → FR-006
- Retry idempotent/safe upstream errors with exponential backoff + jitter, capped attempts, respecting context. Source: US-007.

# processed: 2026-06-24 → FR-007
- Circuit breaker per provider (`sony/gobreaker`); open on repeated failures, fast-fail while open. Source: US-008.

# processed: 2026-06-24 → FR-008
- `GET /healthz` liveness — always 200 while the process is alive. Source: US-009.

# processed: 2026-06-24 → FR-009
- `GET /readyz` readiness — 503 when Redis or Postgres is unavailable. Source: US-010.

# processed: 2026-06-24 → FR-010
- `GET /metrics` Prometheus endpoint. Source: US-011.

# processed: 2026-06-24 → EXT-001 (Provider interface modeled as external system)
- `Provider` interface with a configurable mock implementation (controllable latency/error rate). Source: vision (v1 scope).

# processed: 2026-06-24 → FR-014
- Persist per-request usage records (provider, tokens, latency, status) asynchronously to Postgres via pgx, off the request path. Source: US-015.

# processed: 2026-06-24 → TERM-001, FR-004
- API key parsed from `Authorization` header as the rate-limit / usage key only (no validation backend). Source: vision Non-goals.

## Non-functional (NFR)

# processed: 2026-06-24 → NFR-001
- Sustained throughput: several thousand RPS with gateway p95 overhead < ~10–20 ms (excluding model latency), at the declared hardware (NFR, performance, p95). Source: requirements.md §7.

# processed: 2026-06-24 → NFR-002
- Under overload (2–3× capacity): no crash; latency bounded; excess shed via `429` + `Retry-After`; recovers after load subsides (NFR, availability/operability). Source: requirements.md §7.

# processed: 2026-06-24 → NFR-003
- Zero goroutine leaks under load — verified by pprof before/after (NFR, maintainability). Source: requirements.md §7.

# processed: 2026-06-24 → NFR-004
- Timeouts on every boundary: `http.Server` Read/Write/Idle, upstream client, Redis, Postgres — no unbounded waits (NFR, operability). Source: requirements.md §6.

# processed: 2026-06-24 → NFR-006, FR-015
- Bounded concurrency to upstream with backpressure (semaphore or `errgroup.SetLimit`); goroutine count stays bounded under load (NFR, scalability). Source: US-016.

# processed: 2026-06-24 → NFR-005, FR-012
- Graceful shutdown drains in-flight requests on SIGINT/SIGTERM, logs "drained N requests" (NFR, operability). Source: US-013.

# processed: 2026-06-24 → CON-001 (connection pooling as stack constraint)
- Connection pooling: reused `http.Client` with tuned `Transport` (MaxIdleConns…), pgx pool (NFR, performance). Source: requirements.md §6.

# processed: 2026-06-24 → NFR-007, FR-010, FR-011
- Observability: Prometheus metrics (`http_requests_total`, `http_request_duration_seconds` histogram, `gateway_inflight_requests`, `provider_request_duration_seconds`, `ratelimit_rejected_total`, `breaker_state`) + OTel traces + slog with request-id (NFR, observability). Source: requirements.md §7.

# processed: 2026-06-24 → NFR-008
- Tests run with `-race`; table-driven; real Postgres/Redis via testcontainers (NFR, maintainability). Source: requirements.md §3.

## Constraints (CON)

# processed: 2026-06-24 → CON-001
- Language/stack fixed: Go 1.23+, stdlib-first (`net/http`, `log/slog`), no heavy web framework, no "magic" ORM (CON, technical). Source: requirements.md §3, §9.

# processed: 2026-06-24 → CON-002
- No exotic dependencies; minimal "magic" (CON, technical). Source: requirements.md §9.

# processed: 2026-06-24 → CON-003
- One service, not microservices; idiomatic directory layout per §4 (CON, organizational). Source: requirements.md §4, §9.

# processed: 2026-06-24 → CON-004
- CI: GitHub Actions running build + test `-race` + golangci-lint (CON, technical). Source: requirements.md §3.

# processed: 2026-06-24 → CON-005
- `make up` brings the whole stack up (gateway + postgres + redis + prometheus + grafana) via docker-compose (CON, technical). Source: requirements.md §3.
