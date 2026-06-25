# sluice — developer harness
#
# Hand-written targets may be added ABOVE or BELOW the managed block below;
# they survive `/forge:harness` regeneration. Do not edit inside the markers.

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
