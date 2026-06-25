# User Stories — sluice

## LLM Gateway v1 (2026-06-24)

US-001: As an API Client, I want to send a chat-completion request to `POST /v1/chat/completions` and get the model's response, so that I have a single reliable entry point to LLM inference.

US-002: As an API Client, I want to receive the response as a streamed SSE (`"stream": true`), so that I can render tokens incrementally with low time-to-first-token.

US-003: As an API Client, I want my request routed to the correct provider based on the requested model, so that I don't need to know which upstream serves which model.

US-004: As an API Client, I want my request cancellation (closed connection) to abort the upstream call, so that no work or cost is wasted on a response I no longer need.

US-005: As an API Client, I want a fast `429`/`503` with `Retry-After` when I exceed my rate limit or the gateway is overloaded, so that I can back off instead of hanging.

US-006: As an API Client, I want identical requests served from cache within a TTL, so that repeated calls are faster and cheaper.

US-007: As an API Client, I want transient upstream failures retried with backoff, so that a brief provider hiccup doesn't surface as an error to me.

US-008: As an API Client, I want a failing provider to be short-circuited by a circuit breaker, so that I get a fast failure instead of waiting on a dead upstream.

US-009: As an Operator, I want `GET /healthz` to report liveness, so that the orchestrator can restart a dead process.

US-010: As an Operator, I want `GET /readyz` to report readiness based on Redis and Postgres health, so that traffic is only routed to me when my dependencies are available.

US-011: As an Operator, I want `GET /metrics` exposing RPS, latency histograms, in-flight count, rate-limit rejections, and breaker state, so that I can see load and degradation in Grafana.

US-012: As an Operator, I want end-to-end OpenTelemetry traces of a request through the gateway, so that I can diagnose where latency is spent.

US-013: As an Operator, I want the gateway to drain in-flight requests on SIGINT/SIGTERM before exiting (graceful shutdown), so that no in-flight request is dropped on deploy.

US-014: As an Operator, I want a panic in any handler to be recovered and logged (not crash the process), so that one bad request can't take down the service.

US-015: As an Operator, I want per-request usage records (provider, tokens, latency, status) persisted asynchronously, so that I can account for usage without slowing the request path.

US-016: As an Operator, I want bounded concurrency to upstream providers with backpressure when the worker pool is saturated, so that goroutines don't grow without limit under load.

US-017: As an Operator, I want structured logs (slog) with request-id, latency, and status per request, so that I can correlate and investigate requests.
