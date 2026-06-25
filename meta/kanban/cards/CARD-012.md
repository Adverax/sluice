# CARD-012: OpenAPI contract & codegen scaffolding

**Status:** ready
**Priority:** P1
**Category:** enabler
**Estimate:** 1d
**Revision pending:** false
**Skill:** golang-pro
**TDD:** —
**Branch:** card/012-openapi-contract-codegen
**Worktree:** —
**Source:** ADR-0011
**Depends on:** CARD-002
**Review score:** —
**Started:** —
**Closed:** —
**Actual:** —
**Merge commit:** —
**Blocked by:** —

## What to implement

Establish the contract-first foundation (ADR-0011) that the handler cards (CARD-003 non-stream,
CARD-004 streaming) build on. **No business logic** — just the spec, the codegen pipeline, and
the generated package compiling cleanly.

1. **`api/openapi.yaml`** — OpenAPI **3.0.3** spec, the single source of truth for the public API:
   - `POST /v1/chat/completions` — request schema (`model` required string, `messages` array of
     `{role,content}`, optional `stream` bool, `max_tokens`, `temperature`); 200 response schema
     (`model`, `content`/choices, `usage{prompt_tokens,completion_tokens,total_tokens}`); error
     schema (JSON `{error,...}`) for 400/404/429/500/502/503. Document that `stream:true` yields
     `text/event-stream` (SSE behavior implemented in CARD-004; OpenAPI 3.0 can't fully express it — note it).
   - `GET /healthz` — 200 `{status}`.
   - `GET /readyz` — 200 with per-dependency status; 503 when a dependency is down.
   - `GET /metrics` — Prometheus exposition (text/plain), referenced for completeness.
   - Keep schemas aligned with the canonical `provider.Request`/`Response`/`Usage` shapes so the
     CARD-003 mapping is mechanical (but these are the PUBLIC DTOs, mapped via the ACL — ADR-0009).

2. **oapi-codegen v2 pipeline:**
   - Add `oapi-codegen` config (`oapi-codegen.yaml`) targeting the **stdlib `net/http` server**
     generator (`std-http-server`) + `models` + `strict-server`, output package `internal/api`.
   - A `//go:generate` directive and a tools entry (e.g. `tools.go` build tag) pinning
     `github.com/oapi-codegen/oapi-codegen/v2`.
   - Generate `internal/api/` — request/response/error types, `StrictServerInterface`, and a
     `Handler`/`HandlerFromMux` that registers routes on a `*http.ServeMux`. Commit the generated files.

3. **Build wiring:**
   - Add a **`make generate`** target (runs `go generate ./...`); document it in the Makefile help.
   - Raise `go.mod` to **`go 1.24`** (pgx dependency tree requires it); `go mod tidy`.

## Acceptance criteria

This is enabler scaffolding; "done" = the contract exists, generates reproducibly, and compiles.

**AC-G1**
- **Given:** the repository at this card's branch
- **When:** `make generate` (or `go generate ./...`) is run
- **Then:** `internal/api/` is (re)generated with no diff on a clean tree, and `go build ./...` succeeds
- **Test:** `TestOpenAPISpec_IsValid` — loads `api/openapi.yaml` with `kin-openapi` and asserts `Validate()` returns no error

**AC-G2**
- **Given:** the generated `internal/api` package
- **When:** the project compiles
- **Then:** `StrictServerInterface` and the request/response/error types for `POST /v1/chat/completions` are present and exported
- **Test:** `TestGeneratedAPI_HasChatCompletionsContract` — compile-time references + a smoke assertion that the generated types/interface exist (e.g. `var _ api.StrictServerInterface = ...` once a stub is available, or reflectively assert the operation method exists)

## Architecture context

- **FR:** FR-001, FR-002 (the API surface these endpoints serve)
- **NFR:** —
- **ADR:** ADR-0011 (contract-first), ADR-0009 (DTO↔canonical ACL), CON-001 (stdlib-first → std-http-server)
- **Components:** COMP-001 HTTP Handler & Router (the generated boundary it implements)
- **Trace:** meta/architecture/trace.yml

## Worktree notes

—
