# Vision — sluice (LLM Gateway)

## Summary

`sluice` is a high-performance inference proxy in front of LLM providers. It accepts
client requests, routes them to providers, and adds a reliability and observability
layer: rate limiting, response caching, retries, circuit breaking, streaming (SSE),
and metrics/traces.

This is a **reference repository**: its purpose is to convince a technical decision-maker
(CTO / VP Eng / tech lead) within 5 minutes of reading that the author is a genuine
high-load Go engineer. Every decision serves one of the five signals below.

## Job & Value

- **For whom:** client applications that need a reliable, unified entry point to multiple
  LLM providers, and operators who run this layer under load.
- **Value:** a thin, predictable layer in front of AI workloads that does not fail
  under overload, produces honest latency figures (p95/p99), and degrades gracefully.
- **Key outcome:** sustained several-thousand RPS with gateway-only p95 overhead
  < ~10–20 ms, no crashes, and no goroutine leaks at 2–3× capacity overload.

## Five Signals (the frame for every decision)

1. **Real concurrency** — bounded concurrency, backpressure, cancellation via `context`.
2. **Production-readiness** — graceful shutdown, timeouts at every boundary, panic recovery, health/readiness.
3. **Observability** — metrics and traces that demonstrate operational thinking.
4. **Numbers under load** — honest load test with p95/p99 and overload behaviour.
5. **Code taste** — idiomatic Go, stdlib over heavy frameworks, tests with `-race`.

Any feature that does not strengthen at least one of the five signals does not belong in the product.

## Key Actors

- **API Client** (consumer application) — sends inference requests via
  `POST /v1/chat/completions`, identified by an API key in the `Authorization` header.
- **Operator / SRE** — observes the service via `/metrics`, `/healthz`, `/readyz`
  and a Grafana dashboard; responds to overload and degradation.
- **LLM Provider** (external system) — the upstream that executes inference. In v1 — a
  mock with controllable latency/errors behind the `Provider` interface (extensibility proven by the interface).

## The World the Service Lives In

- **Redis** — distributed rate-limit state and response cache.
- **PostgreSQL** (via `pgx/v5`) — asynchronous write of per-request usage metrics
  (provider, tokens, latency, status). Non-blocking write path; demonstrates the pgx
  connection pool and provides a real dependency for `/readyz`.
- **Prometheus + Grafana** — metrics and dashboard. **OpenTelemetry** — end-to-end traces.

## Stack (fixed by the specification)

Go 1.23+, `net/http` (ServeMux 1.22+) or `chi`, `pgx/v5`, `go-redis/v9`, `log/slog`,
Prometheus client, OpenTelemetry, `golang.org/x/time/rate`, `golang.org/x/sync/errgroup`
(with `SetLimit`), `sony/gobreaker`, k6/vegeta for load testing, stdlib `testing` +
`testcontainers-go`, Makefile + docker-compose, golangci-lint, GitHub Actions.

## Non-goals (explicitly out of scope)

- **Real authentication / provider identity.** The API key in v1 is only an identifier
  for rate limiting and usage metrics — it is not validated against a store.
  Full auth is a "what to add for production" item, not v1.
- **Frontend / React dashboard.** Observability is via Grafana, not a custom UI.
- **Splitting into microservices.** One clean service, not a zoo.
- **Heavy web framework or "magic" ORM.** Against Go taste.
- **Multiple real providers / multi-region / persistent queue.** Listed in the README
  as "what to add for production" but not implemented in v1.
- **Inflated load numbers.** Honest measurements on the stated hardware only.

## Scope Discipline

The goal is to complete this in a few days. The build proceeds in layers (see §10 of the
specification), each layer being a working state and a separate commit. A clean commit
history is itself a signal.

_Source: docs/requirements.md (full specification), 2026-06-24._
