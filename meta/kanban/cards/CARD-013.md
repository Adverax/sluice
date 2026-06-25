# CARD-013: HTTP provider adapter + pooled client (exercise connection pooling)

**Status:** done
**Priority:** P1
**Category:** feature
**Estimate:** 2d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/013-http-provider-pooled-client
**Worktree:** —
**Source:** doc/requirements-audit.md (gap #4: connection pooling unexercised + real-upstream ctx cancellation)
**Depends on:** CARD-003, CARD-002
**Review score:** 9.5 (1 cycle; 0 critical/important; AC-013a–d ✓; pooling proven via httptrace)
**Started:** 2026-06-25T13:18:50Z
**Closed:** 2026-06-25T13:49:24Z
**Actual:** 0.1d
**Merge commit:** fa69f1b
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

The audit found connection pooling (a §6 must-show pattern) is coded but DEAD on the hot path: the
tuned `http.Client` is built in `cmd/gateway` then discarded (`_ = httpClient`), because the only
`Provider` is the in-process `Mock` that makes no HTTP calls. Close this WITHOUT adding a real
third-party provider (real providers remain a v1 non-goal) — by making the mock reachable over REAL
HTTP through the pooled client:

1. **HTTP provider adapter (`internal/provider/httpprovider.go`):** a `Provider` implementation that
   calls an upstream over HTTP using an INJECTED `*http.Client` (the tuned, pooled client from
   `cmd/gateway` — MaxIdleConns/MaxIdleConnsPerHost/IdleConnTimeout, explicit `Timeout`). It maps the
   canonical `Request` → an upstream HTTP request and the upstream response → canonical `Response`/`Chunk`
   (ACL, ADR-0009). Supports both `Infer` (unary JSON) and `InferStream` (reads an SSE/chunked body and
   emits `Chunk`s). The request `ctx` is passed to `http.NewRequestWithContext` so client cancellation
   aborts the REAL upstream call (FR-003 — the spec's literal "cancel aborts the upstream HTTP call").
2. **Mock upstream HTTP server (`internal/provider/mockupstream.go` or under a testutil pkg):** a small
   `http.Handler` (mountable via `httptest.Server` in tests, and runnable as an in-process server in
   `cmd/gateway` for local/load use) that emulates an LLM provider with controllable latency + error rate
   + streaming (SSE) — the reproducible mock the spec recommends, but served over real HTTP so the pool is
   used. Keep the in-process `Mock` (interface-level) for fast unit tests; this adds the HTTP path.
3. **Wire `cmd/gateway`:** build the tuned `http.Client` (stop discarding it), construct the
   `httpProvider` against it pointing at the mock-upstream URL (config `GATEWAY_UPSTREAM_URL`, default the
   in-process mock-upstream the gateway starts on a side port, or a configurable URL), and register it in
   the router for the mock model — so the actual request path now makes pooled HTTP calls. The resilience
   seam (pool→retry→breaker) wraps it unchanged.

ADR-0009 (Provider ACL — no upstream wire types leak), ADR-0010 (injected client). NFR-004: the pooled
client keeps its explicit timeout. Honor scope: this is a MOCK over HTTP, not a real provider integration.

## Acceptance criteria

**AC-013a — pooled client is actually used**
- Given: the gateway wired with the httpProvider over the tuned pooled `*http.Client`
- When: a unary request is proxied
- Then: the upstream is reached via the injected pooled client (no `_ = httpClient`); a test asserts the
  httpProvider issues the request through the provided client (e.g. a `RoundTripper` spy records the call)
- Test: `TestHTTPProvider_UsesInjectedPooledClient`

**AC-013b — connection reuse**
- Given: the httpProvider + a mock upstream, keep-alive enabled
- When: N sequential requests are made
- Then: connections are reused (assert via `httptrace.GotConn.Reused` or a connection-count on the test
  server) — demonstrating pooling
- Test: `TestHTTPProvider_ReusesConnections`

**AC-013c — real-upstream ctx cancellation**
- Given: a mock upstream with 500ms latency
- When: the client cancels the context ~100ms in
- Then: the in-flight upstream HTTP call is aborted (returns context.Canceled) within ~50ms — a REAL
  network call aborted, not an in-process select
- Test: `TestHTTPProvider_ContextCancel_AbortsUpstreamHTTP`

**AC-013d — streaming over HTTP**
- Given: the mock upstream streams SSE chunks
- When: InferStream is called
- Then: chunks are forwarded as they arrive; client disconnect aborts the upstream stream
- Test: `TestHTTPProvider_Stream_ForwardsAndCancels`

## Architecture context

- **FR:** FR-001, FR-003 (real-upstream cancellation)
- **NFR:** NFR-004 (timeout on the upstream client), NFR-001 (pooling keeps overhead low under load)
- **ADR:** ADR-0009, ADR-0010
- **Components:** COMP-005 Provider (new HTTP adapter), COMP-002 Proxy Core
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
