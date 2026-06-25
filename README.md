# sluice

**A production-grade LLM gateway in Go** — a single, contract-first HTTP front
door for chat-completion providers that adds the resilience and observability
patterns you need before putting an LLM in front of real traffic.

> **Performance:** p95 **gateway overhead ≈ 11 µs** (5,000-request in-process
> measurement against a 0-latency mock, Apple M5 Pro / Go 1.26) — roughly three
> orders of magnitude under the 20 ms NFR-001 budget. See
> [`load/RESULTS.md`](load/RESULTS.md) for the methodology and the (pending)
> full-stack k6 numbers measured via `make load`.

---

## Architecture

sluice is a contract-first gateway: the HTTP boundary is generated from
[`api/openapi.yaml`](api/openapi.yaml) (oapi-codegen, ADR-0011, no web
framework), and every cross-cutting concern is a composable middleware or a
decorator over a single `InferFunc` seam. The composition root is
[`cmd/gateway/main.go`](cmd/gateway/main.go).

The C4 model lives in [`meta/architecture/c4/`](meta/architecture/c4/):

- [`context.puml`](meta/architecture/c4/context.puml) — system context.
- [`containers.puml`](meta/architecture/c4/containers.puml) — the gateway,
  Postgres, Redis, Prometheus/Grafana, and providers.
- [`components-proxy.puml`](meta/architecture/c4/components-proxy.puml),
  [`components-resilience.puml`](meta/architecture/c4/components-resilience.puml),
  [`components-metering.puml`](meta/architecture/c4/components-metering.puml),
  [`components-observability.puml`](meta/architecture/c4/components-observability.puml)
  — component-level views.

Request path (outermost first), as wired in `cmd/gateway/main.go`:

```
recover → logging → tracing → metrics → rate-limit → in-flight count → cache → routes
                                                                                   │
                                                              router → pool → retry → breaker → provider
```

---

## Quickstart

### Five-minute demo (full stack)

Bring up the full stack (gateway + Postgres + Redis + Prometheus + Grafana):

```sh
make up
# gateway    -> http://localhost:8080
# metrics    -> http://localhost:8080/metrics
# prometheus -> http://localhost:9090
# grafana    -> http://localhost:3000  (anonymous admin; "sluice gateway" dashboard)
```

`make up` builds the gateway image (multi-stage [`Dockerfile`](Dockerfile),
distroless final image), applies [`migrations/`](migrations/) via a one-shot
`migrate` step, and wires Prometheus to scrape `/metrics` and Grafana to
provision the dashboard automatically.

### Dev loop (host-run gateway)

For iterative development, use `make run` — it starts only Postgres + Redis
(no dockerised gateway), then runs the gateway as a host process:

```sh
make run
# postgres + redis come up first, then:
# go run ./cmd/gateway   (host process on :8080 — no port conflict)
```

Use `make down` to tear down the full demo stack, or `make infra-down` to stop
the infra-only containers started by `make run`.

### Non-streaming completion

```sh
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "mock",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

```json
{"model":"mock","content":"this is a mock completion","finish_reason":"stop",
 "usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}
```

### Streaming completion (SSE)

```sh
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "mock",
    "stream": true,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

```
data: {"content":"this is a mock completion"}

data: {"done":true,"usage":{...}}

data: [DONE]
```

### Health

```sh
curl http://localhost:8080/healthz   # liveness  → 200 {"status":"ok"}
curl http://localhost:8080/readyz    # readiness → 200 when redis+postgres are up, else 503
```

---

## Production patterns demonstrated

Each pattern maps to the package/file that implements it:

| Pattern | Where | Notes |
|---------|-------|-------|
| **Rate limiting** | [`internal/ratelimit/`](internal/ratelimit/), [`internal/middleware/ratelimit.go`](internal/middleware/ratelimit.go) | Local token bucket + **distributed** Redis fixed-window (atomic Lua, shared cross-instance cap, AC-013); fails open to local on a Redis blip. |
| **Ephemeral upstream credentials** | [`cmd/gateway/main.go`](cmd/gateway/main.go) (`newUpstreamClient`), [`internal/provider/`](internal/provider/) | Provider credentials live behind the ACL boundary (ADR-0009) and are attached per upstream call by the provider adapter — never logged, never on the client-facing contract. |
| **Retries (exponential backoff + jitter)** | [`internal/proxy/retry/`](internal/proxy/retry/) | Bounded attempts; treats an open breaker as non-retryable. |
| **Circuit breaker** | [`internal/breaker/`](internal/breaker/), [`internal/proxy/resilience/`](internal/proxy/resilience/) | Per-provider `gobreaker`; open → fast-fail 503 + Retry-After. |
| **Bounded worker pool / backpressure** | [`internal/pool/`](internal/pool/) | Reject-before-work semaphore caps concurrent upstream calls; saturation → 503 (never blocks, never leaks goroutines). |
| **Response cache** | [`internal/cache/`](internal/cache/), [`internal/middleware/cache.go`](internal/middleware/cache.go) | Redis-backed, per-request TTL; a cache error falls through to the live handler (never a client error). |
| **Async metering** | [`internal/metering/`](internal/metering/) | Non-blocking enqueue (drop-on-full) → bounded buffer → batch pgx INSERT into `usage_events`; the hot path never blocks on Postgres. |
| **Graceful shutdown** | [`internal/lifecycle/`](internal/lifecycle/) | Drains in-flight requests, then runs shutdown hooks (flush the metering buffer) under their own deadlines. |
| **Panic recovery** | [`internal/middleware/recover.go`](internal/middleware/recover.go) | Outermost middleware: a handler panic → 500, process survives. |
| **Metrics + tracing** | [`internal/metrics/`](internal/metrics/), [`internal/tracing/`](internal/tracing/), [`internal/middleware/tracing.go`](internal/middleware/tracing.go) | 7 Prometheus series at `/metrics` (requests, latency, in-flight, provider latency, rate-limit rejections, breaker state, dropped metering events) + OTLP spans. |

---

## Development

| Command | What it does |
|---------|--------------|
| `make build` | `go build ./...` |
| `make test` | `go test -race ./...` (unit suite, race detector) |
| `make test-integration` | `go test -tags=integration -race -p 1 ./...` — real Postgres + Redis via testcontainers |
| `make lint` | `go vet` + `golangci-lint run` (staticcheck, errcheck, govet, …) |
| `make up` / `make down` | Full demo stack up/down (gateway + postgres + redis + prometheus + grafana) |
| `make run` | Host dev loop: starts postgres + redis only (`make infra`), then `go run ./cmd/gateway` — no dockerised gateway, no `:8080` conflict |
| `make infra` / `make infra-down` | Start/stop local backing infra only (postgres + redis), without the gateway container |
| `make load` | Run the k6 load scenario ([`load/scenario.js`](load/scenario.js)) against the running stack |
| `make generate` | Regenerate the OpenAPI boundary (must stay diff-clean) |

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) enforces build,
`go test -race`, golangci-lint, a `go generate` diff-clean check, and the
testcontainers integration suite on every PR.

### Tests

- **Unit suite** is race-free and fully hermetic (repos tested against fakes).
- **Integration suite** ([`internal/integration/`](internal/integration/),
  build tag `integration`) spins up **real** Postgres + Redis with
  testcontainers-go and exercises the deferred paths: pgx batch INSERT + read
  back, Redis cache round-trip, the distributed rate-limit Lua across two repo
  instances, and the live readiness 200→503 transition when a container stops.
- **Load/overhead** ([`load/`](load/)) measures gateway overhead in-process
  (p95 ≈ 11 µs) and provides the k6 scenario for the full-stack run.
