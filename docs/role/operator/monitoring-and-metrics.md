# Monitoring and Metrics

## Prometheus metrics endpoint

The gateway exposes all metrics at:

```
GET http://localhost:8080/metrics
```

Prometheus (included in `make up`) scrapes this endpoint every 5 seconds with the
job label `sluice-gateway` and the service label `sluice`.

## Metric catalog

The gateway publishes eight Prometheus series. Each table row names the metric,
its type, its labels, and what it tells you.

### http_requests_total

| | |
|---|---|
| Type | Counter |
| Labels | `route`, `status` |

Counts every completed HTTP request by route and HTTP status code.

Use it to track request volume, error rates, and status-code distribution. A rising
`status="429"` count means clients are hitting the rate limit. A rising `status="503"`
count means the worker pool is saturated or the circuit breaker is open.

### http_request_duration_seconds

| | |
|---|---|
| Type | Histogram |
| Labels | `route` |

Records gateway-side request latency in seconds (the full round trip including
upstream time).

Use it to watch p50/p95/p99 latency. The NFR budget is 20 ms of gateway overhead on
top of upstream latency.

### gateway_inflight_requests

| | |
|---|---|
| Type | Gauge |
| Labels | none |

The number of requests currently being served.

Watch this alongside the worker pool size (`GATEWAY_WORKER_POOL_SIZE`, default 100).
When `gateway_inflight_requests` approaches the pool size, the gateway will start
returning 503 to new requests (reject-before-work backpressure).

### provider_request_duration_seconds

| | |
|---|---|
| Type | Histogram |
| Labels | `provider` |

Upstream provider call latency in seconds, by provider name.

Distinguishes gateway overhead from upstream latency. If
`http_request_duration_seconds` p95 rises but `provider_request_duration_seconds`
p95 is stable, the problem is inside the gateway. If both rise together, the upstream
is slow.

### ratelimit_rejected_total

| | |
|---|---|
| Type | Counter |
| Labels | none |

Counts every 429 rejection from the rate limiter.

A sustained rate of rejections means API clients are sending more than
`GATEWAY_RATELIMIT_RPS` requests per second per key. Consider raising the RPS limit
or informing clients to back off.

### breaker_state

| | |
|---|---|
| Type | Gauge |
| Labels | `provider` |

Current circuit-breaker state per provider. Values: `0` = closed (normal),
`1` = half-open (probing recovery), `2` = open (fast-failing with 503).

Alert when `breaker_state{provider="..."}` reaches `2` — the gateway is rejecting all
requests to that provider. The breaker reopens after `GATEWAY_BREAKER_TIMEOUT`
(default 60s) and allows `GATEWAY_BREAKER_MAX_REQUESTS` (default 5) probe requests
through in half-open state.

### metering_events_dropped_total

| | |
|---|---|
| Type | Counter |
| Labels | none |

Counts usage events dropped because the metering buffer was full.

Usage metering is best-effort by design: the hot path never blocks waiting for
Postgres. Events are dropped rather than slowing requests when the buffer fills up.
A non-zero rate here means the metering worker cannot flush fast enough. Increase
`GATEWAY_METERING_BUFFER_SIZE` or reduce `GATEWAY_METERING_FLUSH_INTERVAL`.

### metering_buffer_size

| | |
|---|---|
| Type | Gauge |
| Labels | none |

Current number of usage events waiting in the buffer to be persisted.

Watch this against `GATEWAY_METERING_BUFFER_SIZE` (default 1000). When the gauge
approaches the buffer size, dropped events become likely.

## Grafana dashboard

The `make up` stack auto-provisions a Grafana dashboard named **"sluice gateway"**
at http://localhost:3000. No login is required — anonymous access is enabled at
admin level.

The dashboard contains seven panels, one per key metric:

| Panel | Metric |
|-------|--------|
| HTTP requests/s (by status) | `http_requests_total` |
| HTTP request latency p50/p95/p99 | `http_request_duration_seconds` |
| In-flight requests | `gateway_inflight_requests` |
| Provider request latency p95 (by provider) | `provider_request_duration_seconds` |
| Rate-limit rejections/s (429) | `ratelimit_rejected_total` |
| Circuit-breaker state | `breaker_state` |
| Metering events dropped/s | `metering_events_dropped_total` |

## OTel tracing

The gateway emits OpenTelemetry spans. Set `GATEWAY_OTEL_ENDPOINT` to your
collector's OTLP/HTTP endpoint (for example `otel-collector:4318`). If the endpoint is
not reachable the gateway continues serving normally — tracing degrades gracefully
and does not affect request handling.
