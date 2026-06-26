# sluice — Requirements

## Service description

sluice is a self-hosted **LLM gateway** — a single HTTP proxy in front of LLM providers
(OpenAI, Anthropic, local models). Applications call one OpenAI-compatible endpoint, and the
gateway handles the cross-cutting concerns that would otherwise be reimplemented in every
service: routing, per-key rate limiting, caching, usage metering, resilience, and streaming.

**Problem it solves.** Once LLM traffic comes from more than one application, team, or key,
the same needs recur: cap the request rate per key, protect against (or switch away from) a
degraded provider, record usage for analytics, keep latency bounded under load, and deliver
streaming uniformly. Building this into each service is fragile and loses central visibility
and control.

**What it does.**
- Exposes a **single OpenAI-compatible endpoint** for all LLM traffic — existing clients only
  change the base URL.
- **Routes** each request to a provider by its `model`.
- Applies **per-key rate limits** (local + distributed), issuing an ephemeral key to keyless
  callers so no one shares a single global bucket.
- **Caches** identical requests (with a TTL) to cut cost and latency.
- Streams responses over **Server-Sent Events**.
- Adds **resilience** — retries, a per-provider circuit breaker, and timeouts at every
  boundary — degrading gracefully when a provider or dependency is unavailable.
- **Meters usage** (tokens, latency, status) per request to a durable ledger, asynchronously
  and off the request hot path.
- Is **observable** — Prometheus metrics, OpenTelemetry traces, structured logs, and
  liveness/readiness probes.

**Audience.** Teams running LLM features in production that want one place to control rate
limits, routing, caching, usage accounting, and reliability — without rebuilding it in every
service.

## Functional requirements

### Inference proxying
- Expose a single OpenAI-compatible endpoint, `POST /v1/chat/completions`, accepting a JSON
  body with at least `model` and `messages`.
- Route each request to a provider based on its `model` field.
- Support both a regular JSON response and a **streaming** response (`"stream": true`)
  delivered as Server-Sent Events, flushed chunk by chunk as they arrive.
- When the client disconnects or cancels the request, the in-flight upstream call must be
  cancelled — no orphaned work.

### Rate limiting
- Enforce a per-API-key request rate using a token bucket, with a local tier and a
  Redis-backed distributed tier shared across instances.
- Identify the key from the `Authorization` header. A caller without a key is issued an
  ephemeral key (returned in a response header and a cookie) so it gets its own bucket
  instead of sharing a single global one.
- When the limit is exceeded, reject immediately with `429 Too Many Requests` and a
  `Retry-After` header — never block.

### Caching
- Optionally serve identical requests from a Redis-backed response cache with a time-to-live,
  including a per-request TTL override. A cache failure must fall through to the live path and
  never surface as a client error. Streaming responses are not cached.

### Usage metering
- Record one usage event per request — provider, model, prompt/completion/total tokens,
  latency, status, request id, timestamp — to PostgreSQL. Recording is asynchronous and must
  never block or fail the request (see the non-functional requirements).

### Operational endpoints
- `GET /healthz` — liveness; returns `200` whenever the process is running.
- `GET /readyz` — readiness; checks Redis and PostgreSQL, returning `503` when a dependency is
  unavailable and `200` otherwise.
- `GET /metrics` — Prometheus exposition.

## Non-functional requirements

### Concurrency and backpressure
- Bound the number of concurrent upstream calls with a worker pool. When the pool is
  saturated, shed load immediately with `503` + `Retry-After` — no unbounded goroutine growth,
  no hangs. Streaming requests count toward the same concurrency bound.
- No goroutine leaks under sustained load or under cancellation.

### Resilience
- Retry transient upstream failures with exponential backoff and jitter, only for safe
  errors, with a capped number of attempts and respect for request cancellation.
- Protect each provider with a circuit breaker: after a threshold of failures it opens and
  fails fast without calling the provider, then recovers through a half-open probe.
- Apply explicit timeouts at every boundary — the HTTP server (read/write/idle), the upstream
  client, PostgreSQL, and Redis. No unbounded waits.
- Under overload (2–3× capacity) the service must not crash: latency stays bounded, excess is
  rejected with a valid status, and throughput recovers after the spike.

### Lifecycle
- Shut down gracefully on `SIGINT`/`SIGTERM`: stop accepting new requests, drain in-flight
  requests, perform a final flush of buffered usage events, then close connection pools.
- A panic in a request handler must not crash the process: it is recovered, logged, and
  returned as `500`.

### Observability
- Export Prometheus metrics, at minimum:
  - `http_requests_total{route,status}` — request rate and errors
  - `http_request_duration_seconds` (histogram) — latency p50/p95/p99
  - `gateway_inflight_requests` (gauge) — current concurrency
  - `provider_request_duration_seconds{provider}` — upstream latency
  - `ratelimit_rejected_total` — rate-limited requests
  - `breaker_state{provider}` — circuit-breaker state
  - `metering_buffer_size`, `metering_events_dropped_total` — async-metering health
- Emit end-to-end traces (OpenTelemetry) and structured logs carrying request id, latency,
  and status. A failure of the trace collector must not affect request handling.

### Performance
- Sustain several thousand requests per second with a gateway overhead (excluding model
  latency) of roughly **p95 < 10–20 ms**, with p99 bounded.
- Under normal load, lose no usage events (`metering_events_dropped_total` = 0).
- Performance figures must be measured honestly on stated hardware, with the method and
  environment recorded alongside the numbers.

## Data and storage requirements
- **PostgreSQL — durable, off the hot path.** Holds the usage-event ledger. Writes happen
  asynchronously, in batches, from a background worker — never synchronously on the request
  path.
- **Redis — ephemeral, on the hot path.** Holds the live rate-limit token buckets and the
  response cache.
- On buffer overflow, metering degrades by dropping events (counted in a metric) rather than
  blocking requests.

## Constraints
- A single service, not a set of microservices.
- Standard-library HTTP (`net/http`); no heavy web framework and no "magic" ORM.
- Go 1.23 or later; PostgreSQL and Redis as the backing stores.
- No synchronous database writes on the request hot path.

## Out of scope (this version)
The following are intentionally not implemented:
- Real third-party provider integrations — a controllable mock upstream is used (which also
  keeps load tests reproducible).
- API-key validation or an authentication backend — the key is used only for rate-limit
  bucketing and usage metering.
- Budgets, quotas, and cost accounting.
- Multi-tenant control-plane data (tenants, plans, routing rules) and provider fallback routing.
