# sluice

[![CI](https://github.com/adverax/sluice/actions/workflows/ci.yml/badge.svg)](https://github.com/adverax/sluice/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

**A production-grade, OpenAI-compatible LLM gateway in Go** — a single drop-in
`/v1/chat/completions` front door (point any OpenAI SDK, or a local Ollama, at it)
that adds the resilience and observability patterns you need before putting an LLM
in front of real traffic.

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
| **The original spec** | [`docs/requirements.md`](docs/requirements.md) — the brief this was built to |

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

sluice speaks the **real OpenAI `/v1/chat/completions` wire format** (ADR-0012),
so it is a drop-in for the OpenAI SDKs and `curl` — only the base URL changes.

```sh
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "mock",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

```json
{
  "id": "chatcmpl-9f8c1a2b3c4d5e6f70819a2b",
  "object": "chat.completion",
  "created": 1718000000,
  "model": "mock",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "this is a mock completion"},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
}
```

The request is **liberal-accept**: unknown OpenAI fields (`seed`, `user`,
`presence_penalty`, `frequency_penalty`, `logit_bias`, `response_format`, `n`,
`logprobs`, …) are accepted and ignored — never a 400. `n>1`, multimodal/array
content, and function calling are documented non-goals (CON-008) and return an
OpenAI-shaped 400.

You can also point the official OpenAI SDK at sluice unmodified:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-anything")
resp = client.chat.completions.create(
    model="mock",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.choices[0].message.content)
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
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1718000000,"model":"mock","choices":[{"index":0,"delta":{"content":"this "}}]}

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1718000000,"model":"mock","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

Errors use the OpenAI envelope so SDKs parse them:

```json
{"error": {"message": "no provider is registered for model x", "type": "invalid_request_error", "code": "model_not_found"}}
```

### Health

```sh
curl http://localhost:8080/healthz   # liveness  → 200 {"status":"ok"}
curl http://localhost:8080/readyz    # readiness → 200 when redis+postgres are up, else 503
```

### Real OpenAI-compatible backend (optional)

By default `make up` uses the **in-process mock** upstream, so the demo is fast
and pulls no models. To proxy a real OpenAI-compatible backend, set the upstream
env vars (the URL must include the `/v1` segment):

| Backend | `GATEWAY_UPSTREAM_URL` | `GATEWAY_UPSTREAM_MODEL` | Key |
|---------|------------------------|--------------------------|-----|
| Ollama | `http://ollama:11434/v1` (compose) or `http://localhost:11434/v1` (host) | `llama3.2` | none |
| OpenAI | `https://api.openai.com/v1` | `gpt-4o-mini` | `GATEWAY_UPSTREAM_API_KEY=sk-…` |
| vLLM / LM Studio | `http://host:port/v1` | your model | optional |

An optional **Ollama** service ships behind the `ollama` compose profile (it is
NOT started by default):

```sh
# start the stack WITH Ollama
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.stack.yml \
  --profile ollama up -d

# one-time model pull
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.stack.yml \
  exec ollama ollama pull llama3.2
```

Then run the gateway against it (host process):

```sh
GATEWAY_UPSTREAM_URL=http://localhost:11434/v1 \
GATEWAY_UPSTREAM_MODEL=llama3.2 \
go run ./cmd/gateway
```

Because the edge is wire-compatible, the same `curl`/SDK calls above work
unchanged — just use `"model": "llama3.2"`.

---

## Production patterns demonstrated

Each pattern maps to the package/file that implements it:

| Pattern | Where | Notes |
|---------|-------|-------|
| **OpenAI-compatible API + real upstream** | [`internal/server/edge.go`](internal/server/edge.go), [`internal/provider/httpprovider.go`](internal/provider/httpprovider.go) | Drop-in `/v1/chat/completions` (liberal-accept request, `chat.completion`/`chat.completion.chunk` responses, OpenAI error envelope, edge-generated `id`); the upstream adapter speaks the same wire to Ollama / OpenAI / vLLM (ADR-0012/0013). |
| **Rate limiting** | [`internal/ratelimit/`](internal/ratelimit/), [`internal/middleware/ratelimit.go`](internal/middleware/ratelimit.go) | Local token bucket + **distributed** Redis token-bucket (atomic Lua, shared cross-instance cap, AC-013); fails open to local on a Redis blip. |
| **Ephemeral API keys** | [`internal/middleware/ratelimit.go`](internal/middleware/ratelimit.go) | A keyless caller is minted a crypto-random key (returned via `X-Sluice-Api-Key` + cookie) and gets its own rate-limit bucket — no noisy-neighbour, no auth backend required (ADR-0001). |
| **Retries (exponential backoff + jitter)** | [`internal/proxy/retry/`](internal/proxy/retry/) | Bounded attempts; treats an open breaker as non-retryable. |
| **Circuit breaker** | [`internal/breaker/`](internal/breaker/), [`internal/proxy/resilience/`](internal/proxy/resilience/) | Per-provider `gobreaker`; open → fast-fail 503 + Retry-After. |
| **Bounded worker pool / backpressure** | [`internal/pool/`](internal/pool/) | Reject-before-work semaphore caps concurrent upstream calls; saturation → 503 (never blocks, never leaks goroutines). |
| **Response cache** | [`internal/cache/`](internal/cache/), [`internal/middleware/cache.go`](internal/middleware/cache.go) | Redis-backed, per-request TTL; a cache error falls through to the live handler (never a client error). |
| **Async metering** | [`internal/metering/`](internal/metering/) | Non-blocking enqueue (drop-on-full) → bounded buffer → batch pgx INSERT into `usage_events`; the hot path never blocks on Postgres. |
| **Graceful shutdown** | [`internal/lifecycle/`](internal/lifecycle/) | Drains in-flight requests, then runs shutdown hooks (flush the metering buffer) under their own deadlines. |
| **Panic recovery** | [`internal/middleware/recover.go`](internal/middleware/recover.go) | Outermost middleware: a handler panic → 500, process survives. |
| **Metrics + tracing** | [`internal/metrics/`](internal/metrics/), [`internal/tracing/`](internal/tracing/), [`internal/middleware/tracing.go`](internal/middleware/tracing.go) | 8 Prometheus series at `/metrics` (requests, latency, in-flight, provider latency, rate-limit rejections, breaker state, metering buffer size, dropped metering events) + OTLP spans. |

---

## Tech decisions

A few choices that define the repo's character (the full rationale lives in
[`meta/architecture/decisions/`](meta/architecture/decisions/adr/)):

- **`net/http` + ServeMux, no framework** (CON-001). Go's 1.22+ ServeMux covers the routing
  this gateway needs; a framework would add magic and dependency weight for no real gain.
- **Contract-first OpenAPI + `oapi-codegen`** (ADR-0011). `api/openapi.yaml` is the single
  source of truth; the typed server boundary is generated onto the plain ServeMux and requests
  are schema-validated at the edge — the discipline of a contract without the weight of a framework.
- **OpenAI-compatible, end-to-end** (ADR-0012/0013). Both the public edge and the upstream adapter
  speak the real OpenAI `/v1/chat/completions` wire, so any OpenAI SDK is a drop-in and the same
  gateway fronts Ollama / OpenAI / vLLM by config. The request is *liberal-accept* — unknown OpenAI
  fields are ignored, never a 400; only the modeled subset is forwarded, and `id`/`created` are
  generated at the edge.
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

- **Provider breadth & routing** — sluice already proxies any **OpenAI-compatible** backend
  (Ollama, OpenAI, vLLM, LM Studio) via the real upstream adapter; a real deployment would add native
  adapters for non-OpenAI wires (e.g. Anthropic's native API) behind the same `Provider` interface,
  plus **multi-provider routing by model** (today: a single configured upstream). The default `make up`
  upstream is a controllable in-process mock so the demo stays fast and load tests stay reproducible.
- **Fallback-provider routing** on an open breaker (multi-provider failover) — today a tripped breaker
  fast-fails; with multiple providers it would reroute.
- **Real authentication** — v1 treats the API key as an identifier only (no validation backend), plus a
  per-source-IP cap on ephemeral-key issuance to close the rate-limit bypass.
- **Billing-grade metering durability** — a WAL / Kafka in front of Postgres so usage is never dropped
  under overload (v1 is best-effort drop-on-full).
- **Multi-region deployment and persistent configuration** for routes / per-key limits (today limits are
  env-configured; the distributed rate-limit tier is already a Redis token bucket).

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
