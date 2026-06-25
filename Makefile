# sluice — developer harness
#
# Hand-written targets may be added ABOVE or BELOW the managed block below;
# they survive `/forge:harness` regeneration. Do not edit inside the markers.

.PHONY: generate

generate: ## Regenerate code from the OpenAPI spec (oapi-codegen, ADR-0011)
	go generate ./...

# generated_by: forge:harness — do not edit inside the managed block; edit meta/.skills.yml or run /forge:harness
# >>> forge:harness (managed) >>>
.DEFAULT_GOAL := help

# --- Config (override via env or `make VAR=...`) ---------------------------
COMPOSE                  := deploy/docker-compose.yml
GATEWAY_DB_DSN          ?= postgres://app:app@localhost:5432/sluice?sslmode=disable
GATEWAY_REDIS_URL       ?= redis://localhost:6379
GATEWAY_WORKER_POOL_SIZE ?= 100

export GATEWAY_DB_DSN GATEWAY_REDIS_URL GATEWAY_WORKER_POOL_SIZE

.PHONY: help build run test lint fmt tidy up down logs ps ci clean

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Compile all packages
	go build ./...

run: up ## Run the gateway locally (brings up infra first)
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

up: ## Start local backing infra (postgres + redis)
	docker compose -f $(COMPOSE) up -d

down: ## Stop local infra and remove containers
	docker compose -f $(COMPOSE) down

logs: ## Tail infra logs
	docker compose -f $(COMPOSE) logs -f

ps: ## Show infra container status
	docker compose -f $(COMPOSE) ps

ci: lint build test ## Local CI gate (mirrors forge:ci / CON-004: build + test -race + lint)

clean: ## Remove build/test artifacts
	go clean ./...
	rm -rf bin/
# <<< forge:harness (managed) <<<

# --- CARD-011 full-stack + load + integration (CON-005) ---------------------
# Hand-written targets BELOW the managed block. Make uses the LAST definition of
# a target, so these OVERRIDE the managed `up`/`down` to bring up the FULL stack
# (gateway + postgres + redis + prometheus + grafana) by layering
# deploy/docker-compose.stack.yml on top of the managed deploy/docker-compose.yml
# — the managed region itself is never edited.
.PHONY: up down test-integration load migrate stack-logs

STACK_OVERLAY := deploy/docker-compose.stack.yml
COMPOSE_FILES := -f $(COMPOSE) -f $(STACK_OVERLAY)
LOAD_SCENARIO ?= load/scenario.js
LOAD_TARGET   ?= http://localhost:8080

up: ## Bring up the FULL stack: gateway + postgres + redis + prometheus + grafana (CON-005)
	docker compose $(COMPOSE_FILES) up -d --build
	@echo "gateway    -> http://localhost:8080"
	@echo "metrics    -> http://localhost:8080/metrics"
	@echo "prometheus -> http://localhost:9090"
	@echo "grafana    -> http://localhost:3000 (anonymous admin)"

down: ## Tear down the full stack and its volumes
	docker compose $(COMPOSE_FILES) down -v

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
