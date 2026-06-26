# sluice

[![CI](https://github.com/adverax/sluice/actions/workflows/ci.yml/badge.svg)](https://github.com/adverax/sluice/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

**A production-grade LLM gateway in Go** — a single, contract-first HTTP front
door for chat-completion providers that adds the resilience and observability
patterns you need before putting an LLM in front of real traffic.

> **Performance** (Apple M5 Pro / Go 1.26 — see [`load/RESULTS.md`](load/RESULTS.md) for full method):
> - **Pure gateway overhead p95 ≈ 11 µs** — 5,000-request in-process measurement against a 0-latency mock; ~1000× under the 20 ms NFR-001 budget.
> - **Full-stack p95 ≈ 1–5 ms** (k6 over loopback, real Redis + Postgres, 0 ms mock upstream) at sustainable load — well under 20 ms.
> - **Graceful degradation:** 432k requests across a ramp → 3k → **9k** overload → recovery run, **0 failures, 0 panics**, every response 200/429/503 (NFR-002).
>
> ```
> full-stack p95 latency vs load (single laptop, load-gen + gateway + mock co-resident)
>   500 rps │■■■■■  4.1 ms          ┐
>   700 rps │■■■■■■  5.5 ms         │ below the ~850 rps rig knee → real service time
>   800 rps │■  1.0 ms              ┘
>  1000 rps │ saturated → ~1.8 s    ← load-gen queueing (rig ceiling, NOT gateway:
>  2000 rps │ saturated → ~2.0 s      pure gateway work is ~11 µs/req, far from the bottleneck)
> ```
> _The ~850 rps ceiling is the co-resident test rig (k6 + gateway + mock on one machine, double loopback per request), not the gateway. A real throughput benchmark needs the load generator and upstream on separate hosts._

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

## Documentation

| For | Read |
|-----|------|
| **Using the API** | [`docs/role/integrator/`](docs/role/integrator/) — requests, streaming, rate limits, errors, API reference |
| **Operating it** | [`docs/role/operator/`](docs/role/operator/) — running the stack, health, the metric catalog, tuning, shutdown |
| **How it works inside** | [`docs/how-it-works/`](docs/how-it-works/) — layered, code-grounded mechanism docs per aspect (with diagrams) |
| **Why it's built this way** | [`meta/`](meta/README.md) — ADRs, C4 diagrams, the domain model, requirements traceability, and the build log |
| **The original spec** | [`doc/requirements.md`](doc/requirements.md) — the brief this was built to |

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
| **Ephemeral API keys** | [`internal/middleware/ratelimit.go`](internal/middleware/ratelimit.go) | A keyless caller is minted a crypto-random key (returned via `X-Sluice-Api-Key` + cookie) and gets its own rate-limit bucket — no noisy-neighbour, no auth backend required (ADR-0001). |
| **Retries (exponential backoff + jitter)** | [`internal/proxy/retry/`](internal/proxy/retry/) | Bounded attempts; treats an open breaker as non-retryable. |
| **Circuit breaker** | [`internal/breaker/`](internal/breaker/), [`internal/proxy/resilience/`](internal/proxy/resilience/) | Per-provider `gobreaker`; open → fast-fail 503 + Retry-After. |
| **Bounded worker pool / backpressure** | [`internal/pool/`](internal/pool/) | Reject-before-work semaphore caps concurrent upstream calls; saturation → 503 (never blocks, never leaks goroutines). |
| **Response cache** | [`internal/cache/`](internal/cache/), [`internal/middleware/cache.go`](internal/middleware/cache.go) | Redis-backed, per-request TTL; a cache error falls through to the live handler (never a client error). |
| **Async metering** | [`internal/metering/`](internal/metering/) | Non-blocking enqueue (drop-on-full) → bounded buffer → batch pgx INSERT into `usage_events`; the hot path never blocks on Postgres. |
| **Graceful shutdown** | [`internal/lifecycle/`](internal/lifecycle/) | Drains in-flight requests, then runs shutdown hooks (flush the metering buffer) under their own deadlines. |
| **Panic recovery** | [`internal/middleware/recover.go`](internal/middleware/recover.go) | Outermost middleware: a handler panic → 500, process survives. |
| **Metrics + tracing** | [`internal/metrics/`](internal/metrics/), [`internal/tracing/`](internal/tracing/), [`internal/middleware/tracing.go`](internal/middleware/tracing.go) | 7 Prometheus series at `/metrics` (requests, latency, in-flight, provider latency, rate-limit rejections, breaker state, dropped metering events) + OTLP spans. |

---

## Tech decisions

A few choices that define the repo's character (the full rationale lives in
[`meta/architecture/decisions/`](meta/architecture/decisions/adr/)):

- **`net/http` + ServeMux, no framework** (CON-001). Go's 1.22+ ServeMux covers the routing
  this gateway needs; a framework would add magic and dependency weight for no real gain.
- **Contract-first OpenAPI + `oapi-codegen`** (ADR-0011). `api/openapi.yaml` is the single
  source of truth; the typed server boundary is generated onto the plain ServeMux and requests
  are schema-validated at the edge — the discipline of a contract without the weight of a framework.
- **Reject-before-work backpressure** (worker pool as a non-blocking semaphore, ADR-0003).
  When the pool is full the request is rejected *immediately* (503 + `Retry-After`) — never queued.
  A queue would mask overload and grow goroutines without bound; immediate shedding keeps latency
  and memory flat under 3× load.
- **`retry(breaker(provider))` composition** (ADR-0006). The breaker sits *inside* retry, so an
  open breaker fast-fails without burning retry attempts, and we never retry into a tripped breaker —
  the failure signals "switch provider," not "hammer harder."
- **Async, drop-on-full metering** (ADR-0007). Usage events go to a bounded channel with a
  non-blocking send; the hot path never waits on Postgres. Under overload, events are dropped and
  counted (`metering_events_dropped_total`) rather than slowing requests — best-effort by design.
- **Rate limiter fails *open*** on a Redis error. A proxy's job is availability; a Redis blip
  must not 503 the whole fleet, so enforcement degrades to the local token bucket and logs a warning.
- **`pgx`/`go-redis` directly, no ORM** (ADR-0010). Explicit SQL and an injected repository
  interface per context — no hidden queries, swappable client, testable without a live DB.

## What I'd add for production

This is a focused reference build; the honest gaps a real deployment would close (most are scored in
[`meta/kanban/backlog.md`](meta/kanban/backlog.md)):

- **Real provider adapters** (OpenAI / Anthropic) behind the existing `Provider` interface — v1 ships
  a controllable *mock* upstream (over real HTTP, so connection pooling and cancellation are genuinely
  exercised), which keeps load tests reproducible.
- **Real authentication** — v1 treats the API key as an identifier only (no validation backend), plus a
  per-source-IP cap on ephemeral-key issuance to close the rate-limit bypass.
- **Fallback-provider routing** on an open breaker (multi-provider failover) — today a tripped breaker
  fast-fails; with multiple providers it would reroute.
- **Billing-grade metering durability** — a WAL / Kafka in front of Postgres so usage is never dropped
  under overload (v1 is best-effort drop-on-full).
- **Sliding-window distributed rate limiting** (v1's Redis tier is a fixed window), multi-region
  deployment, and persistent configuration for routes / per-key limits.

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
