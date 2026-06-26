# Go Reference Repository Spec

**Goal:** a single public repository that convinces a technical buyer (CTO / VP Eng / tech lead) at a product, fintech, iGaming, or crypto company within 5 minutes of reading that you are a genuine high-load Go engineer — not "Golang on a resume." It also serves as your entry into the "AI infrastructure on Go" niche.

This is not a tutorial project or a "todo app in Go." Every decision below is chosen to land on a specific signal that buyers filter for (section 2).

---

## 1. Service Description (for end users)

> Substitute your chosen name — e.g. `sluice` or `inference-gateway`. The text below goes into the README header.

**LLM Gateway** — a self-hosted proxy in front of LLM providers (OpenAI, Anthropic, local models). Your application calls a single OpenAI-compatible endpoint, and the gateway handles everything you would otherwise have to reimplement in every service: routing between providers, per-key rate limits and budgets, usage and cost accounting, caching, retries, and streaming.

**The problem it solves.** As soon as LLM requests come from more than one application / team / key, the same concerns appear every time: limit the rate and budget per key, have one place to switch or fall back to another provider, count tokens and cost, avoid failing when a provider degrades, and deliver streaming uniformly. Baking this into every service is fragile and you lose centralized visibility and control.

**What it does:**
- **Single OpenAI-compatible endpoint** for all LLM traffic — existing clients only need to change the base URL.
- **Request routing** to the right provider / model, with fallbacks.
- **Rate limits, quotas, and monthly budgets** per API key.
- **Per-request accounting** — tokens, cost, latency — for billing and analytics.
- **Cache** for identical requests: lower cost and latency.
- **Resilience** — retries, circuit breaker, timeouts; graceful degradation when a provider is unavailable.
- **Streaming** responses (SSE).
- **Observability** — Prometheus metrics and traces.

**Who it is for.** Teams with LLM features in production that need centralized control over cost, limits, routing, and reliability — without building that into every service separately.

---

## 2. What the Repository Must Prove

A technical buyer grep-s (mentally and literally) your repo for 5 signals. Build everything around them:

1. **Real concurrency** — not `go func()` for the sake of it, but bounded concurrency, backpressure, and cancellation via `context`.
2. **Production-readiness** — graceful shutdown, timeouts at every boundary, panic recovery, health/readiness probes, separation of control plane / data plane.
3. **Observability** — metrics and traces that show you think about operations.
4. **Numbers under load** — an honest load test with p95/p99 and behavior under overload.
5. **Code taste** — idiomatic Go, stdlib over heavy frameworks, clean structure, tests with `-race`.

If a feature does not strengthen one of these five signals, it does not go into the repo (section 11 "What NOT to do").

---

## 3. Why This Domain (LLM Gateway) for a Portfolio

- The domain maps exactly to your target niche — "high-performance Go layer in front of AI workloads."
- It naturally exercises all the patterns Go engineers are hired for: high concurrency, backpressure, streaming (SSE), connection pools, timeouts, retries, circuit breaker, cache, async metering, latency metrics.
- It produces impressive load numbers — exactly what a buyer wants to see.
- The patterns are 100% transferable: the same skeleton works as a fintech / iGaming / SaaS backend. In conversation say: "the domain is an AI gateway, but the concurrency, backpressure, and resilience are identical to what you find in a payment or betting backend."

> If you want a more domain-neutral angle: the same skeleton works as a high-throughput analytics API or a task-queue service. But the gateway hits both of your niches at once, which is why it is recommended.

---

## 4. Tech Stack (idiomatic and defensible)

Stack choice is itself a taste signal — Go culture values stdlib and lightness, not "a framework for everything."

| Layer | Choice | Why |
|-------|--------|-----|
| Language | Go 1.23+ | current, generics, `slog`, improved `net/http` ServeMux |
| HTTP | `net/http` (ServeMux 1.22+) or `chi` | no heavy web frameworks — a plus in the eyes of Go people |
| Source of truth | PostgreSQL via `pgx/v5` | control plane: keys/tariffs + usage ledger (off the hot path) |
| Cache / rate-limit state | Redis (`go-redis/v9`) | data plane: live limits and response cache |
| Logs | `log/slog` (stdlib) | structured logging without external dependencies |
| Metrics | Prometheus client + Grafana | RPS, latency, in-flight, errors |
| Traces | OpenTelemetry | end-to-end request tracing |
| Rate limiting | `golang.org/x/time/rate` + Redis | token bucket locally + distributed |
| Concurrency | `golang.org/x/sync/errgroup` (with `SetLimit`) | bounded fan-out |
| Circuit breaker | `sony/gobreaker` | protection against a failing provider |
| Load testing | `k6` or `vegeta` | reproducible scenarios + report |
| Tests | stdlib `testing` + `testcontainers-go` | table-driven, real Postgres/Redis in tests |
| Build/run | `Makefile` + `docker-compose` | `make up` brings everything up with one command |
| Lint | `golangci-lint` | in CI |
| CI | GitHub Actions | build + test `-race` + lint |

No exotic dependencies. The less "magic," the stronger the signal.

---

## 5. Directory Structure

Idiomatic, without over-engineering. Do not split into 15 microservices — that is an anti-signal at this stage.

```
llm-gateway/
├── cmd/
│   └── gateway/
│       └── main.go            # entry point: config, DI, graceful shutdown
├── internal/
│   ├── server/                # HTTP layer: router, middleware
│   │   ├── server.go
│   │   ├── handlers.go        # /v1/chat/completions, /healthz, /readyz, /metrics
│   │   └── middleware.go      # recovery, logging, metrics, timeout, ratelimit
│   ├── proxy/                 # core: routing to providers
│   │   ├── router.go          # provider selection
│   │   ├── provider.go        # Provider interface + implementations
│   │   ├── stream.go          # SSE streaming with flush
│   │   └── pool.go            # worker pool / bounded concurrency
│   ├── resilience/            # retries, backoff, circuit breaker
│   │   ├── retry.go
│   │   └── breaker.go
│   ├── ratelimit/             # token bucket (local + redis)
│   ├── cache/                 # response cache (redis)
│   ├── metering/              # usage-event buffer + batch flusher to Postgres
│   ├── store/                 # Postgres access (pgx): api_keys, usage, routing
│   ├── observability/         # metrics, tracing, logger setup
│   └── config/                # config loading/validation (env)
├── migrations/                # SQL migrations (api_keys, usage_events, ...)
├── load/                      # k6/vegeta scenarios + results
│   ├── scenario.js
│   └── RESULTS.md             # numbers + charts
├── deploy/
│   ├── docker-compose.yml     # gateway + postgres + redis + prometheus + grafana
│   └── grafana/               # dashboard (json)
├── Makefile
├── Dockerfile                 # multi-stage, small final image
├── .github/workflows/ci.yml
└── README.md                  # the main selling document (section 10)
```

---

## 6. Functionality (specific endpoints)

The minimum needed to prove the signals — no more.

- `POST /v1/chat/completions` — the main proxy. Supports:
  - regular and **streaming** (`"stream": true`) responses via SSE;
  - routing by provider (model field → provider);
  - per-API-key rate limiting (`Authorization` header);
  - cache for identical requests (optional, with TTL).
- `GET /healthz` — liveness (always 200 if the process is alive).
- `GET /readyz` — readiness (checks Redis/Postgres; 503 if a dependency is unavailable).
- `GET /metrics` — Prometheus.

Providers: one real one (or a mock with controllable latency/errors — which is actually better for a reproducible load test) + an interface to show extensibility.

---

## 7. Storage: Postgres (control plane) + Redis (data plane)

The split is itself a maturity signal: you understand what belongs in durable relational storage and what belongs in ephemeral hot storage.

**Postgres — source of truth (control plane), off the hot path:**

- `tenants` / `api_keys` — keys (store the **hash**, not the key itself), tenant, plan/tier, allowed models and providers, limits, monthly budget, status. The ground truth for "who this is and what they are allowed to do." Redis is warm-loaded from this data on startup and on changes.
- `usage_events` — per-request record: timestamp, key, model, provider, prompt/completion tokens, cost, latency, status, request_id. Ledger for billing, quotas, and analytics — the most natural job of the gateway (metering).
- (optional) `routing_rules` — model → provider and fallbacks, to change routing without a redeploy.
- (optional) `audit_log` — who / what / when; an extra plus for fintech buyers.

**Redis — hot path (data plane):** live token buckets for rate limits, response cache, short-lived quota counters. Everything that must be fast and ephemeral.

**Critical:** do **not** write to Postgres synchronously on the request path — doing so puts a DB write on the hot path and the load test numbers will collapse. Metering is asynchronous, in batches (pattern in section 8). A time-series DB is not needed: batch inserts handle the volume easily.

---

## 8. What the Code Must Demonstrate (these are the proofs)

This is the heart of the repo. Each item is a distinct signal; make them explicit and readable.

- **Context propagation + cancellation** — `context.Context` threaded through the entire path; cancellation of the client request cancels the upstream call. Show that a timeout/cancellation actually stops work.
- **Bounded concurrency / worker pool** — a pool with a limit (buffered channel semaphore or `errgroup.SetLimit`). When exhausted: backpressure, not unbounded goroutine growth.
- **Backpressure under overload** — when the queue is full: a fast `429`/`503` with `Retry-After`, not a hang. Key item — shows you think about overload.
- **Async metering (batch flush)** — usage events are NOT written to Postgres synchronously on the hot path. They are placed into a bounded in-memory buffer (buffered channel) and flushed **in batches** by a background worker (by batch size or by timer, whichever comes first). On buffer overflow: metering degradation (drop with counter), not request blocking. Another concurrency showcase.
- **Rate limiting** — token bucket (`x/time/rate`) locally + Redis for distributed. Per-key limits.
- **Retries with exponential backoff + jitter** — only for idempotent/safe errors, with a retry cap and respect for `context`.
- **Circuit breaker** — per provider; opens after a series of errors, does not continue hammering a failed upstream.
- **Timeouts at EVERY boundary** — `http.Server` (Read/Write/Idle), upstream client, DB, Redis. No unbounded waits.
- **Streaming with flush** — `http.Flusher`, correct SSE delivery, stream cancellation when the client disconnects.
- **Connection pooling** — reusable `http.Client` with a configured `Transport` (MaxIdleConns, etc.), `pgx` pool.
- **Graceful shutdown** — `signal.NotifyContext` (SIGINT/SIGTERM) → `server.Shutdown(ctx)` → drain in-flight requests → **final metering buffer flush** → close pools. With a log line "drained N requests, flushed M usage events."
- **Panic recovery middleware** — a panic in a handler does not crash the process; it is logged + `500`.
- **Structured logs** — `slog` with request-id, latency, status; log levels.

> AI usage tip: scaffold the skeleton and wiring with an assistant, but READ and UNDERSTAND each of these patterns yourself — on a call the buyer will ask "why a semaphore and not unbounded," "what happens when cancellation occurs mid-stream," "why is metering async," and that is where they tell real engineers apart. The repo opens the door; the conversation closes the deal.

---

## 9. Observability and Load Test (the money shot)

### Metrics (Prometheus)
- `http_requests_total{route,status}` — RPS and error rate
- `http_request_duration_seconds` (histogram) — p50/p95/p99
- `gateway_inflight_requests` (gauge) — current load
- `provider_request_duration_seconds{provider}` — upstream latency
- `ratelimit_rejected_total`, `breaker_state{provider}`
- `metering_buffer_size`, `metering_events_dropped_total` — async metering health

Grafana dashboard (json in repo), screenshot in README.

### Load Test
Scenario in `load/scenario.js` (k6): ramp up to target RPS, plateau, then overload (2–3× capacity) — to demonstrate **graceful degradation**.

**Measure numbers honestly on the stated machine** (specify CPU/RAM/environment — e.g. "8 vCPU, 16 GB, local, upstream mock with fixed 50 ms latency"). Inflated numbers are spotted instantly by a technical buyer and destroy trust.

Target figures (gateway overhead only, excluding model latency):
- sustain **several thousand RPS** with **p95 overhead < ~10–20 ms**, p99 within bounds;
- under overload — **no crash**: latency is bounded, excess is rejected with `429` + `Retry-After`, and throughput recovers after the spike;
- 0 goroutine leaks (show a `pprof` snapshot before/after — a strong bonus);
- metering does not lose events under normal load (`metering_events_dropped_total` = 0).

In `load/RESULTS.md`: launch command, environment, table (RPS / p50 / p95 / p99 / error rate), and chart. The headline number goes at the top of the README.

---

## 10. README — The Main Selling Document (reads in 5 minutes)

The buyer decides here whether to open your code or close the tab. Fixed structure:

1. **One line**: what it is and why (take from section 1). *"High-throughput LLM gateway in Go: routing, rate limits, metering, streaming, and resilience with p95 overhead ~X ms at N RPS."*
2. **Headline number + chart** right below the header (benchmark from section 9). This is the hook.
3. **Architecture diagram** — simple (mermaid or ASCII): client → gateway (middleware → pool → breaker) → providers; Redis (data plane) and Postgres (control plane) on the side.
4. **Quickstart**: `make up` → curl example for a regular and a streaming request. Must start with one command.
5. **Production patterns demonstrated** — checklist from section 8 with links to specific files. A CTO skimming should see the signals without reading code.
6. **Tech decisions** — 5–7 lines "why net/http and not a framework," "why a semaphore for backpressure," "why metering is async and Postgres is off the hot path." Shows thinking, not copy-paste.
7. **What I'd add for production** — a short honest list (auth provider, multi-region, persistent queue for metering…). Maturity means knowing the limits of a PoC.

Write the README last, but treat it like a landing page — it is what sells.

---

## 11. What NOT to Do (scope discipline)

- Unfinished frontend / React "dashboard" — not your signal, wastes time.
- Splitting into 5 microservices "to look serious" — at this stage that is an anti-signal. One clean service > a zoo.
- A heavy web framework or "magic" ORM — against Go taste.
- Synchronous metering writes to Postgres on the hot path — kills the numbers. Async batch only.
- Exotic dependencies for the wow factor — they reduce trust instead.
- A pile of features "for later." Every feature must strengthen one of the 5 signals. Otherwise — out.
- Inflated / unrealistic load numbers.

The goal is to finish in **a few days**, not to build for six months.

---

## 12. Build Order (to reach done quickly with AI)

Work in layers; each layer is a working state:

1. **Skeleton**: structure, config, `main.go`, `/healthz`, `/readyz`, graceful shutdown, slog. → commit.
2. **Proxy happy path**: `Provider` interface + mock, `POST /v1/chat/completions` (no streaming), timeouts, `http.Client` pool. → commit.
3. **Streaming**: SSE with flush, cancellation via `context`. → commit.
4. **Storage + keys**: migrations, `store` on pgx (`api_keys`), key loading + Redis warm-up. → commit.
5. **Rate limiting**: token bucket local → Redis, per-key, `429` + `Retry-After`. → commit.
6. **Metering**: bounded usage-event buffer + batch flusher to Postgres, flush by size/timer, buffer metrics. → commit.
7. **Resilience**: retries with backoff+jitter, circuit breaker. → commit.
8. **Bounded concurrency / backpressure**: worker pool, behavior under overload. → commit.
9. **Observability**: Prometheus metrics, OTel traces, Grafana dashboard. → commit.
10. **Tests**: table-driven, `-race`, testcontainers (Postgres+Redis); recovery middleware. → commit.
11. **Load**: k6 scenario, capture numbers, `RESULTS.md`, pprof for leaks. → commit.
12. **README + polish**: diagram, headline number, checklist, tech decisions; final linter pass and CI.

A clean, meaningful commit history is itself a signal — buyers browse it.

---

## 13. How to Use the Repo in Sales

- In the first outreach message — **link + headline number**: "built a high-throughput LLM gateway in Go, sustains N RPS at p95 ~X ms, code here: …".
- **Pin** the repo on your GitHub profile and attach it to Djinni / DOU.
- In conversation **reference specific files** ("backpressure here: internal/proxy/pool.go", "async metering: internal/metering/") — this sharply increases trust.
- The repo proves general high-load competency AND positions you in the AI infrastructure niche simultaneously — use both angles with different buyers.

---

### TL;DR
One service — **LLM gateway in Go**. It exercises every pattern Go engineers are hired for (concurrency, backpressure, streaming, resilience, observability, async metering), delivers honest load numbers, and hits both of your niches at once. Postgres — control plane (keys + usage ledger, off the hot path), Redis — data plane (live limits + cache); the split itself is a maturity signal. README = landing page with the headline number at the top. Scope is tight; timeline is days.
