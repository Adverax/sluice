# Health endpoints

The gateway exposes two health endpoints. As an API consumer you can use them
to check whether the gateway is ready to accept your requests.

## GET /healthz — liveness

Returns `200 OK` as long as the gateway process is running.

```sh
curl http://localhost:8080/healthz
```

```json
{"status": "ok"}
```

This endpoint always returns `200` while the process is alive. It does not check
whether Redis or Postgres are reachable.

## GET /readyz — readiness

Returns `200 OK` when all dependencies (Redis and Postgres) are healthy. Returns
`503 Service Unavailable` when at least one dependency is down.

```sh
curl http://localhost:8080/readyz
```

**All dependencies healthy (200):**

```json
{
  "status": "ok",
  "dependencies": {
    "redis": "ok",
    "postgres": "ok"
  }
}
```

**A dependency is down (503):**

```json
{
  "status": "unavailable",
  "dependencies": {
    "redis": "connection refused",
    "postgres": "ok"
  }
}
```

The `dependencies` map contains one entry per checked dependency. The value is
`"ok"` when healthy, or an error description when not.

## When to use these endpoints

- **Before sending inference requests:** poll `/readyz` to confirm the gateway
  and its backing services are up before starting work.
- **Health checks in your infrastructure:** point load-balancer or orchestrator
  liveness probes at `/healthz` and readiness probes at `/readyz`.
- **Diagnosing errors:** if you are seeing unexpected 503 responses from
  `/v1/chat/completions`, check `/readyz` to determine whether a dependency is
  down.

## GET /metrics — Prometheus metrics

The gateway exposes Prometheus metrics at `/metrics` in the standard text
exposition format. This endpoint is primarily for operators and monitoring
systems, but you can query it to inspect request rates, latency distributions,
rate-limit rejection counts, and circuit-breaker state if you need that
information.

```sh
curl http://localhost:8080/metrics
```
