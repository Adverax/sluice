# sluice — developer harness
#
# Hand-written targets may be added ABOVE or BELOW the managed block below;
# they survive `/forge:harness` regeneration. Do not edit inside the markers.

.PHONY: generate

generate: ## Regenerate code from the OpenAPI spec (oapi-codegen, ADR-0011)
	go generate ./...

# generated_by: forge:harness — do not edit inside the managed block; edit meta/.skills.yml or run /forge:harness
# extended for the full make-up stack per CON-005 (CARD-011)
# >>> forge:harness (managed) >>>
.DEFAULT_GOAL := help

# --- Config (override via env or `make VAR=...`) ---------------------------
COMPOSE                  := deploy/docker-compose.yml
STACK_OVERLAY            := deploy/docker-compose.stack.yml
COMPOSE_FILES            := -f $(COMPOSE) -f $(STACK_OVERLAY)
GATEWAY_DB_DSN          ?= postgres://app:app@localhost:5432/sluice?sslmode=disable
GATEWAY_REDIS_URL       ?= redis://localhost:6379
GATEWAY_WORKER_POOL_SIZE ?= 100
LOAD_SCENARIO            ?= load/scenario.js
LOAD_TARGET              ?= http://localhost:8080

export GATEWAY_DB_DSN GATEWAY_REDIS_URL GATEWAY_WORKER_POOL_SIZE

.PHONY: help build run test lint fmt tidy up down infra infra-down logs ps ci clean migrate stack-logs test-integration load

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Compile all packages
	go build ./...

# DEV LOOP: host-run gateway against infra only (postgres+redis). No dockerised
# gateway → no :8080 conflict. Use `make up` for the full demo stack instead.
run: infra ## Run the gateway locally against infra only (host process, no port conflict)
	go run ./cmd/gateway

test: ## Run the unit + testcontainers suite with the race detector (CON-004)
	go test -race ./...

lint: ## Static checks: go vet + golangci-lint (skipped gracefully if absent)
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run \
		|| echo "golangci-lint not installed — skipping (CON-004 requires it in CI)"

fmt: ## Format the codebase
	gofmt -l -w .

tidy: ## Tidy module dependencies
	go mod tidy

# FULL DEMO STACK (CON-005): gateway + postgres + redis + prometheus + grafana.
# One command brings everything up. Use `make run` for the dev loop instead.
up: ## Bring up the FULL stack: gateway + postgres + redis + prometheus + grafana (CON-005)
	docker compose $(COMPOSE_FILES) up -d --build
	@echo "gateway    -> http://localhost:8080"
	@echo "metrics    -> http://localhost:8080/metrics"
	@echo "prometheus -> http://localhost:9090"
	@echo "grafana    -> http://localhost:3000 (anonymous admin)"

down: ## Tear down the full stack and remove volumes
	docker compose $(COMPOSE_FILES) down -v

# INFRA ONLY: postgres + redis (no gateway container → used by `make run`).
infra: ## Start local backing infra only: postgres + redis (used by make run)
	docker compose -f $(COMPOSE) up -d --wait

infra-down: ## Stop local infra (postgres + redis) only
	docker compose -f $(COMPOSE) down

logs: ## Tail infra-only logs
	docker compose -f $(COMPOSE) logs -f

ps: ## Show infra container status
	docker compose -f $(COMPOSE) ps

stack-logs: ## Tail the full-stack logs
	docker compose $(COMPOSE_FILES) logs -f

migrate: ## Apply SQL migrations against the running postgres
	docker compose $(COMPOSE_FILES) run --rm migrate

test-integration: ## Run the testcontainers integration suite with the race detector (AC-049)
	go test -tags=integration -race -p 1 ./...

load: ## Run the k6 load scenario against the running stack (LOAD_TARGET=$(LOAD_TARGET))
	@command -v k6 >/dev/null 2>&1 \
		|| { echo "k6 not installed — see https://k6.io/docs/get-started/installation/"; exit 1; }
	k6 run -e BASE_URL=$(LOAD_TARGET) $(LOAD_SCENARIO)

ci: lint build test ## Local CI gate (mirrors forge:ci / CON-004: build + test -race + lint)

clean: ## Remove build/test artifacts
	go clean ./...
	rm -rf bin/
# <<< forge:harness (managed) <<<
