# Configuration Reference

All gateway behaviour is controlled by environment variables with the `GATEWAY_`
prefix. Every variable has a default, so the gateway boots without any configuration.

If a variable is set to an invalid or non-positive value, the gateway refuses to start
and prints an error — it never silently falls back to the default for a malformed
value.

Duration values use Go duration syntax: `5s`, `500ms`, `2m`, etc.

---

## Server

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_ADDR` | `:8080` | Listen address for the HTTP server. |
| `GATEWAY_READ_TIMEOUT` | `5s` | Maximum time to read a complete request including body. |
| `GATEWAY_WRITE_TIMEOUT` | `10s` | Maximum time to write the response. |
| `GATEWAY_IDLE_TIMEOUT` | `120s` | Maximum keep-alive idle time. |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `30s` | Time budget to drain in-flight requests on shutdown. |

---

## Concurrency and backpressure

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_WORKER_POOL_SIZE` | `100` | Maximum concurrent upstream calls. When the pool is full, new requests receive 503 immediately (reject-before-work — never queued). |

---

## Rate limiting

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_RATELIMIT_RPS` | `10` | Per-API-key token-bucket refill rate (requests per second). |
| `GATEWAY_RATELIMIT_BURST` | `20` | Per-API-key token-bucket capacity (maximum momentary burst). |
| `GATEWAY_RATELIMIT_WINDOW` | `1s` | Window for the distributed (Redis) fixed-window counter. |
| `GATEWAY_RATELIMIT_MAX_KEYS` | `100000` | Maximum number of per-key rate-limit buckets held in memory. Excess keys evict the least-recently-used entry. |

The local token bucket and the distributed Redis counter work together. If Redis is
temporarily unreachable, enforcement falls back to the local bucket only — the gateway
stays available and logs a warning.

---

## Timeouts

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_UPSTREAM_TIMEOUT` | `30s` | Total timeout for a single upstream provider request. |
| `GATEWAY_REDIS_DIAL_TIMEOUT` | `5s` | Timeout for establishing a Redis connection. |
| `GATEWAY_REDIS_READ_TIMEOUT` | `3s` | Timeout for a single Redis read. |
| `GATEWAY_DB_ACQUIRE_TIMEOUT` | `5s` | Timeout to acquire a connection from the Postgres pool. |
| `GATEWAY_HEALTH_CHECK_TIMEOUT` | `3s` | Per-dependency timeout for `/readyz` checks (Redis, Postgres). |

---

## Retry engine

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_RETRY_MAX_ATTEMPTS` | `3` | Total number of tries (1 initial + up to 2 retries). Set to `1` to disable retries. |
| `GATEWAY_RETRY_BASE_DELAY` | `50ms` | Backoff delay for the first retry. Subsequent retries grow exponentially. |
| `GATEWAY_RETRY_MAX_DELAY` | `2s` | Maximum backoff delay cap. |
| `GATEWAY_RETRY_JITTER` | `0.5` | Fraction of the computed delay applied as random jitter (0–1). Spread retries to avoid thundering-herd. |

The retry engine does not retry into an open circuit breaker. An open breaker fast-fails
immediately without consuming retry attempts.

---

## Circuit breaker

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_BREAKER_INTERVAL` | `10s` | Tumbling counter-reset period in the closed state. |
| `GATEWAY_BREAKER_TIMEOUT` | `60s` | Time the breaker stays open before moving to half-open. |
| `GATEWAY_BREAKER_MAX_REQUESTS` | `5` | Probe requests allowed through in half-open state. |
| `GATEWAY_BREAKER_MIN_REQUESTS` | `10` | Minimum request volume before the breaker may trip. |
| `GATEWAY_BREAKER_FAILURE_RATIO` | `0.5` | Failure ratio (0–1) at or above which the breaker trips. |
| `GATEWAY_BREAKER_RETRY_AFTER` | `60s` | Value sent in the `Retry-After` header on 503 fast-fails. |

---

## Response cache

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_CACHE_TTL` | `5m` | Default cache-entry lifetime. Clients can override this per-request with the `X-Cache-TTL` header. |
| `GATEWAY_CACHE_MAX_BODY_BYTES` | `1048576` (1 MiB) | Maximum request body size eligible for caching. Requests with larger bodies are proxied without caching — they do not receive an error. |

---

## Usage metering

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_METERING_BUFFER_SIZE` | `1000` | Channel capacity for the async usage-event buffer. Events dropped when the buffer is full are counted in `metering_events_dropped_total`. |
| `GATEWAY_METERING_FLUSH_INTERVAL` | `5s` | How often the metering worker flushes buffered events to Postgres. The other trigger is the batch filling up. |

---

## Shutdown hooks

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_SHUTDOWN_HOOK_TIMEOUT` | `5s` | Time budget given to each post-drain shutdown hook (e.g. the metering flush). Each hook gets its own independent budget. |

---

## Upstream and mock

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_UPSTREAM_URL` | _(empty)_ | HTTP(S) URL of an external upstream provider. When empty, the gateway starts an in-process mock upstream. |
| `GATEWAY_MOCK_UPSTREAM_ADDR` | `127.0.0.1:0` | Listen address for the in-process mock upstream (used only when `GATEWAY_UPSTREAM_URL` is unset). The default `0` port picks an ephemeral port automatically. |

---

## Connections

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_REDIS_URL` | `redis://localhost:6379` | Redis connection string. |
| `GATEWAY_DB_DSN` | `postgres://app:app@localhost:5432/sluice?sslmode=disable` | Postgres connection string. |

---

## Logging

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, or `error`. |
| `GATEWAY_LOG_FORMAT` | `json` | Log format: `json` (production) or `text` (local development). |

---

## Tracing

| Variable | Default | Effect |
|----------|---------|--------|
| `GATEWAY_OTEL_ENDPOINT` | _(empty)_ | OTLP/HTTP endpoint for OpenTelemetry traces (e.g. `otel-collector:4318`). When empty, tracing is disabled (no-op provider). If the endpoint is unreachable at runtime, the gateway continues normally. |
