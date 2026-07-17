.PHONY: help build vet fmt fmt-check check-import-boundary lint test test-unit up down swagger-ui env run contracts-build contracts-test

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

build: ## Compile everything
	go build ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go source files in place
	gofmt -w .

fmt-check: ## Fail if any Go file is not gofmt-formatted (CI-safe, no writes)
	@test -z "$$(gofmt -l .)" || (echo "not gofmt-formatted:"; gofmt -l .; exit 1)

check-import-boundary: ## Fail if any .go file (including _test.go) outside internal/adapter/evm imports go-ethereum (AD-1)
	@matches=$$(grep -rl 'github\.com/ethereum/go-ethereum' --include='*.go' . | grep -v '^\./internal/adapter/evm/'); \
	if [ -n "$$matches" ]; then \
		echo "go-ethereum imported outside internal/adapter/evm (AD-1 violation):"; \
		echo "$$matches"; \
		exit 1; \
	fi

lint: vet fmt-check check-import-boundary ## vet + fmt-check + check-import-boundary together

test: ## Run the full suite, including the real-Postgres integration test (needs Docker)
	go test ./...

test-unit: ## Run only the fast, no-Docker-required tests
	go test ./... -short

env: ## Create .env from .env.example if it doesn't exist yet (never overwrites)
	@test -f .env || cp .env.example .env

up: ## Start Postgres in Docker (the only container used for local dev)
	docker compose -f deploy/compose/docker-compose.yml up -d postgres

down: ## Stop and remove the local Postgres container
	docker compose -f deploy/compose/docker-compose.yml down

run: env ## Run the API locally against the Dockerized Postgres (loads .env)
	set -a && . ./.env && set +a && go run ./cmd/walletd api

contracts-build: ## Compile the Foundry contracts project (factory + forwarder)
	cd contracts && forge build

contracts-test: ## Run the Foundry test suite (cross-language CREATE2 vectors, AC5)
	cd contracts && forge test
