# Operating Under Load

## Overload behaviour

The gateway sheds load fast rather than queuing or stalling. There are two
independent overload signals:

### Worker pool saturation (503)

When the number of concurrent upstream calls reaches `GATEWAY_WORKER_POOL_SIZE`
(default 100), the gateway returns **503 Service Unavailable** with a
`Retry-After` header to new requests immediately. It never queues the request or
blocks a goroutine. The process stays healthy and continues serving requests that
can acquire a pool slot.

Watch `gateway_inflight_requests`. When it sustains near `GATEWAY_WORKER_POOL_SIZE`,
increase the pool size or add gateway instances.

### Rate limit exceeded (429)

When an API key exhausts its per-second token bucket, the gateway returns **429 Too
Many Requests**. The `ratelimit_rejected_total` counter increments. The process
continues serving other keys normally.

If legitimate traffic is being rejected, increase `GATEWAY_RATELIMIT_RPS` and/or
`GATEWAY_RATELIMIT_BURST`. See [configuration-reference.md](configuration-reference.md).

## Circuit breaker

The circuit breaker protects the gateway from a failing upstream provider. It has
three states, reported by the `breaker_state{provider="..."}` metric:

| Value | State | Behaviour |
|-------|-------|-----------|
| `0` | Closed | Normal — all requests pass through. |
| `1` | Half-open | Provider may be recovering — up to `GATEWAY_BREAKER_MAX_REQUESTS` probe requests are allowed through. |
| `2` | Open | Fast-failing — all requests receive 503 + `Retry-After` immediately, without contacting the provider. |

**How it trips:** Once a provider has received at least `GATEWAY_BREAKER_MIN_REQUESTS`
(default 10) requests in the current `GATEWAY_BREAKER_INTERVAL` (default 10s) window,
the breaker trips if the failure ratio reaches `GATEWAY_BREAKER_FAILURE_RATIO`
(default 0.5, i.e. 50%).

**How it recovers:** After `GATEWAY_BREAKER_TIMEOUT` (default 60s), the breaker moves
to half-open and allows `GATEWAY_BREAKER_MAX_REQUESTS` (default 5) probes through. If
those succeed, it closes. If they fail, it reopens.

**Alert:** Set an alert on `breaker_state == 2` for any provider. An open breaker
means all requests to that provider are failing fast. The upstream or network needs
attention.

## Retries and interaction with the circuit breaker

The retry engine makes up to `GATEWAY_RETRY_MAX_ATTEMPTS` (default 3) total tries
with exponential backoff and jitter. It does not retry into an open breaker — an
open breaker causes an immediate fast-fail without consuming retry budget. Retries
are spaced by `GATEWAY_RETRY_BASE_DELAY` (default 50ms), growing up to
`GATEWAY_RETRY_MAX_DELAY` (default 2s), with `GATEWAY_RETRY_JITTER` (default 0.5)
randomisation to avoid thundering-herd.

## Scaling

The gateway is **stateless**. Run multiple instances behind a load balancer and
they automatically share:

- **Rate limit state** — The distributed Redis fixed-window counter ensures a shared
  per-key cap across all instances. If Redis is temporarily unreachable, each instance
  enforces its local token bucket only (the gateway fails open rather than failing
  all traffic).
- **Response cache** — All instances read from and write to the same Redis cache, so
  a cached response from one instance is served by all.

Postgres (usage events) is append-only and written asynchronously; it does not need
sharding or coordination between instances.

**Sizing guidance:** One instance with the default pool size of 100 worker slots
handles 100 concurrent upstream calls. For higher concurrency, either raise
`GATEWAY_WORKER_POOL_SIZE` or add instances. The p95 gateway overhead is approximately
11 µs under in-process measurement (see `load/RESULTS.md`), so vertical scaling
works well.

## What happens during a Redis outage

- **Rate limiting** degrades to local token buckets — no distributed cap, but requests
  are not rejected wholesale. A warning is logged.
- **Response cache** misses for every request — traffic passes through to the upstream.
- **Readiness probe** (`/readyz`) returns 503 until Redis recovers. This removes the
  instance from load-balancer rotation in environments that honour readiness.
- **Liveness probe** (`/healthz`) remains 200 — the process is alive.

## What happens during a Postgres outage

- **Usage metering** drops events to the metering buffer. When the buffer fills (default
  1000 events), additional events are dropped and counted in `metering_events_dropped_total`.
  Traffic continues normally.
- **Readiness probe** (`/readyz`) returns 503 until Postgres recovers.
