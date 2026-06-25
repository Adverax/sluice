# CARD-013: HTTP Provider over the pooled client (close audit gap #4)

**Status:** in-progress
**Priority:** P2
**Category:** tech-debt
**Estimate:** 0.5d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/013-http-provider-pooled-client
**Worktree:** sluice-card-013
**Source:** requirements-audit.md gap #4 (spec §6 connection pooling)
**Depends on:** CARD-002 (Provider port + Mock), CARD-013 wiring
**Blocked by:** —

## What to implement

Close audit gap #4: connection pooling (a spec §6 must-show) is DEAD because
`cmd/gateway/main.go` builds a tuned `http.Client` then discards it (`_ = httpClient`) —
the only Provider was the in-process Mock (no HTTP). Make the mock reachable over REAL
HTTP through the pooled client so pooling + real-upstream ctx-cancellation are genuinely
exercised. Still a MOCK (no real OpenAI/Anthropic — a v1 non-goal).

1. `internal/provider/httpprovider.go` — an HTTP `Provider` built with an INJECTED
   `*http.Client` + base URL. `Infer` POSTs canonical-mapped JSON and maps the upstream
   JSON → canonical `Response`; `InferStream` reads the SSE body incrementally and emits
   canonical `Chunk`s, closing on completion/error/ctx-cancel. Non-2xx → `*StatusError`
   (5xx retryable / 4xx not). No upstream wire types cross the boundary (ADR-0009).
2. `internal/provider/mockupstream.go` — `MockUpstreamHandler(opts)` served over real HTTP
   (httptest in tests, in-process side-server in the gateway), controllable latency /
   error-rate / SSE stream.
3. Wire `cmd/gateway/main.go`: stop discarding the client; construct the `HTTPProvider`
   against it; resolve the upstream URL via `GATEWAY_UPSTREAM_URL` (external) or start the
   in-process mock upstream on `GATEWAY_MOCK_UPSTREAM_ADDR`; register for the "mock" model;
   stop the side-server on graceful shutdown.
4. Config: `GATEWAY_UPSTREAM_URL` (optional) + `GATEWAY_MOCK_UPSTREAM_ADDR` (default),
   fail-loud.

## Acceptance criteria

**AC-013a** — HTTPProvider routes Infer through the INJECTED client (RoundTripper spy).
**AC-013b** — N sequential Infer calls reuse the pooled connection (httptrace
`GotConn.Reused`). Proves pooling.
**AC-013c** — ctx cancel aborts the in-flight upstream HTTP call promptly and the upstream
observes the client gone.
**AC-013d** — InferStream forwards SSE chunks; cancel stops + aborts upstream; no goroutine
leak.

## Architecture context

- **FR:** FR-001, FR-002, FR-003 (ctx on every call)
- **NFR:** NFR-004 (explicit timeouts), spec §6 connection pooling
- **ADR:** ADR-0009 (single Provider ACL), ADR-0010 (shared pooled client)
- **Components:** COMP-005 Provider port

## Worktree notes

Implemented on branch `card/013-http-provider-pooled-client` (worktree `sluice-card-013`).

> NOTE: the card file `CARD-013.md` and `doc/requirements-audit.md` referenced in the task
> prompt did not exist in the worktree at start; this card file was created from the prompt's
> scope so the worktree notes have a home. The implementation follows the prompt's SCOPE 1–4
> and AC-013a–d verbatim.

**Files created**
- `internal/provider/httpprovider.go` — `HTTPProvider` (`NewHTTP(client, baseURL, opts...)`).
  - ACL: private `wireRequest`/`wireResponse`/`wireStreamEvent`/`wireUsage` (OpenAI-ish wire
    shape) are deliberately DISTINCT from the canonical types and never cross the boundary;
    `toWireRequest`/`toCanonicalUsage`/`toWireUsage` are the explicit mappers.
  - `Infer`: marshals the wire request, `http.NewRequestWithContext` (ctx-bound), POSTs via
    the injected client, drains+closes the body (so the conn returns to the pool), maps a
    non-2xx status → `*StatusError`, decodes the JSON → canonical `Response`.
  - `InferStream`: requests the SSE variant; on a 2xx returns a channel fed by a single
    reader goroutine (`streamLoop`) that scans `data:` lines, unmarshals each into a canonical
    `Chunk`, emits a terminal `Done`+`Usage` chunk, selects on `ctx.Done()` at every send, and
    closes BOTH the body and the channel on exit (`defer`) — no goroutine leak. A `[DONE]`
    sentinel / EOF / ctx-cancel ends the stream; read/decode errors become a terminal `Err`
    chunk.
  - `mapTransportError` surfaces `context.Canceled`/`context.DeadlineExceeded` unwrapped so
    callers/tests match with `errors.Is` (FR-003); other transport errors are wrapped.
- `internal/provider/mockupstream.go` — `MockUpstreamHandler(MockUpstreamOptions)` returning
  an `http.Handler` on `POST /v1/chat/completions` that branches on the wire `stream` flag
  between JSON and `text/event-stream`. Controllable `Latency` (honoured against the request
  ctx via the shared `sleepCtx`), `FailStatus` (error-rate), `StreamChunks`. Flushes after
  every SSE event and stops promptly on client disconnect.
- `internal/provider/httpprovider_test.go` — AC-013a (RoundTripper spy), AC-013b
  (httptrace `GotConn.Reused` across 5 sequential calls, asserts >= n-1 reuses → pooling
  proven), AC-013c (slow upstream + ctx cancel ~100ms → `Infer` returns `context.Canceled`
  in < ~150ms and the handler observes the disconnect via a background body-drain that
  triggers `r.Context().Done()`), AC-013d (SSE forward + cancel-closes-channel, no leak),
  plus a table-driven `*StatusError` 5xx/4xx classification test.
- `internal/provider/mockupstream_test.go` — unit tests for the handler: unary
  defaults/custom/fail-status, SSE delta+Done+[DONE] shape, latency honoured.

**Files modified**
- `cmd/gateway/main.go` — STOP discarding the client (`_ = httpClient` removed). New
  `resolveUpstream(cfg.Upstream, logger)` returns the upstream URL + an optional stop func:
  if `GATEWAY_UPSTREAM_URL` is set it is used verbatim; otherwise an in-process mock upstream
  is started on a loopback side-listener (`net.Listen` on `GATEWAY_MOCK_UPSTREAM_ADDR`,
  default `127.0.0.1:0`) and the resolved `http://host:port` is targeted. The "mock" model is
  now registered as `provider.NewHTTP(httpClient, upstreamURL)` (the in-process `provider.Mock`
  stays for fast unit tests but is no longer on the running gateway's path). The side-server's
  graceful `Shutdown` is registered as a lifecycle `OnShutdown` hook. The resilience seam
  (pool→retry→breaker) wraps the HTTPProvider unchanged.
- `internal/config/config.go` — `Upstream.URL` (`GATEWAY_UPSTREAM_URL`, optional) +
  `Upstream.MockUpstreamAddr` (`GATEWAY_MOCK_UPSTREAM_ADDR`, default `127.0.0.1:0`), with
  fail-loud `Validate` (URL must be a valid absolute http(s) URL with a host; mock addr must
  be non-empty when URL is unset). `TestConfig_AllBoundariesHaveTimeouts` stays green (no new
  timeout fields added to that contract).
- `internal/config/config_test.go` — extra Validate table rows for the new fields.
- `internal/provider/README.md` — documents `HTTPProvider` + the mock upstream.

**Validation**
- `go build ./...` ✅; `go vet ./...` ✅; `golangci-lint run` ✅ (clean).
- `go test -race ./...` ✅ (all 19 packages green).
- `go generate ./...` diff-clean (no change to `internal/api/api.gen.go` or
  `api/openapi.yaml`); `go mod tidy` stable (no new deps — stdlib only).

**AC → test mapping**
- AC-013a → `TestHTTPProvider_UsesInjectedPooledClient`
- AC-013b → `TestHTTPProvider_ReusesConnections` (httptrace `GotConn.Reused`)
- AC-013c → `TestHTTPProvider_ContextCancel_AbortsUpstreamHTTP`
- AC-013d → `TestHTTPProvider_Stream_ForwardsAndCancels`
- classification → `TestHTTPProvider_MapsStatusError`
- mock upstream → `TestMockUpstreamHandler_Unary` / `_Stream` / `_Latency`
