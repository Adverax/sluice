# ADR-0011: API contract-first with OpenAPI + oapi-codegen

## Status

Accepted — 2026-06-25

## Context

`sluice` exposes an HTTP API (`POST /v1/chat/completions`, `GET /healthz`, `/readyz`,
`/metrics`). The original plan hand-wrote request/response DTOs and routing inside the
proxy handler (CARD-003). That approach drifts: the wire contract lives only in code,
clients have no machine-readable spec, and request validation is ad hoc.

`forge:engineering-standards` mandates a contract-first discipline for APIs: the OpenAPI
specification is the source of truth; types and the server boundary are generated from it;
every endpoint has contract tests. This decision adopts that discipline for sluice. It must
stay within CON-001 (stdlib-first, no heavy web framework) and CON-002 (no exotic deps).

## Decision

The HTTP API is **contract-first**:

1. `api/openapi.yaml` (OpenAPI **3.0.3**) is the single source of truth for the public API —
   request/response/error schemas and all paths.
2. Server types and the routing boundary are **generated** with **`oapi-codegen` v2** using
   the **stdlib `net/http` server** generator (`std-http-server`, Go 1.22+ `ServeMux`) — no
   web framework, honoring CON-001. Generated code lives in `internal/api/` (a
   `StrictServerInterface` + types + a `Handler` that registers routes on a `*http.ServeMux`).
3. Generation is reproducible: a `//go:generate` directive plus a **`make generate`** target;
   generated files are committed.
4. Handlers **implement** the generated interface and map generated API DTOs ↔ the canonical
   `provider.Request`/`Response` (ADR-0009 ACL). No hand-written types duplicate the spec.
5. Every endpoint gets contract/handler tests; the spec is validated in CI.

Scaffolding (spec + codegen + generated package) is an enabler — **CARD-012** — that the
handler cards (CARD-003 non-stream, CARD-004 streaming) depend on.

`go.mod` is raised to **go 1.24** (the `pgx/v5` dependency tree requires it).

## Consequences

### Positive
- Single source of truth for the wire contract; clients get a publishable spec.
- Request decoding/validation and routing are generated and consistent, not ad hoc.
- Adding/changing an endpoint starts from the spec → regenerate → implement, reducing drift.
- Aligns the repo with `forge:engineering-standards` (a "code taste" signal per the vision).

### Negative
- A codegen step is now part of the build (`make generate`); stale generated code is a new
  drift class (mitigated: committed + regenerable + CI check).
- `oapi-codegen` is an added dev dependency (tool-only, not a runtime framework — within CON-002).
- Streaming (SSE) is not fully expressible in OpenAPI 3.0; the `stream:true` response is
  documented in the spec but the SSE wire behavior is implemented/tested in code (CARD-004).

### Neutral
- `go 1.24` bump is required regardless by the pgx dependency tree.
- Health/readiness/metrics endpoints are included in the spec for completeness even though
  their bodies are simple.

## Alternatives considered

- **Hand-written handler + DTOs (status quo).** Rejected: contract lives only in code, drifts,
  no client-facing spec, ad-hoc validation — contrary to engineering-standards.
- **A web framework with built-in binding/validation (gin, echo, fiber).** Rejected: violates
  CON-001 (stdlib-first) and the vision's "no heavy framework" stance.
- **Generate from spec into a framework server (oapi-codegen + chi/echo target).** Rejected:
  same framework objection; the stdlib `std-http-server` target keeps it framework-free.
