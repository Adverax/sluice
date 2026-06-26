# Contributing to sluice

Thanks for your interest! sluice is a **reference** LLM gateway — a place to see
production patterns (resilience, observability, contract-first APIs) done idiomatically
in Go. Contributions that keep it clean, correct, and well-documented are very welcome.

## Prerequisites

- **Go 1.25+** (`go.mod` pins the toolchain).
- **Docker** — for the integration suite and the local stack (`make up`, testcontainers).
- **k6** (optional) — only for the load scenario (`make load`).

## Build, test, lint

```sh
make build              # go build ./...
make test               # go test -race ./...           (unit, hermetic — no infra)
make test-integration   # go test -tags=integration -race -p 1 ./...   (real Postgres+Redis via testcontainers)
make lint               # go vet + golangci-lint
make generate           # regenerate the OpenAPI boundary — must stay diff-clean
```

Run the gateway locally:

```sh
make up      # full demo stack: gateway + postgres + redis + prometheus + grafana
make run     # host dev loop: infra in Docker, gateway as a host process on :8080
```

## How this project is built

sluice is **contract-first** and **architecture-first** — please work with the grain:

1. **API changes start from the contract.** Edit [`api/openapi.yaml`](api/openapi.yaml),
   run `make generate`, and commit the regenerated `internal/api/api.gen.go`. CI fails if
   the generated code drifts from the spec. Don't hand-edit generated files.
2. **Significant design changes start from the model.** The architecture lives in
   [`meta/`](meta/README.md) (ADRs, C4, domain model, requirements + traceability). New
   cross-cutting decisions should land as an ADR under
   [`meta/architecture/decisions/adr/`](meta/architecture/decisions/adr/). See
   [`meta/README.md`](meta/README.md) for how the model and the build log are organised.
3. **Keep the boundaries.** The codebase is ports-and-adapters: infrastructure
   (Prometheus, pgx, go-redis) never leaks across a domain boundary — it's injected
   behind a narrow interface. Match the surrounding style; prefer the standard library.

## Pull request checklist

- [ ] `make build`, `make test` (race-clean), and `make lint` pass.
- [ ] `make generate` leaves no diff (contract ↔ generated code in sync).
- [ ] New behavior has tests; integration paths use testcontainers, not mocks-of-infra.
- [ ] Docs updated where relevant ([`docs/how-it-works/`](docs/how-it-works/) for mechanism,
      [`docs/role/`](docs/role/) for user-facing behavior).
- [ ] No fabricated performance numbers — measure honestly or mark pending (see
      [`load/RESULTS.md`](load/RESULTS.md) for the bar).

## Scope

sluice deliberately ships a controllable **mock** upstream rather than real provider
integrations — see the README's "What I'd add for production" for the intentional v1
non-goals. PRs that add real provider adapters behind the existing `Provider` interface,
or that close one of those gaps, are especially welcome.

By contributing, you agree your work is licensed under the repository's
[MIT License](LICENSE).
