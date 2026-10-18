# SoroForge development tasks.
#
# `make help` lists everything.

BINARY      := soroforge
CMD         := ./cmd/soroforge
BIN_DIR     := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

# Matches the docker-compose service; override for another database.
DATABASE_URL ?= postgres://soroforge:soroforge@localhost:5432/soroforge?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD)

.PHONY: install
install: ## Install the binary into GOPATH/bin
	go install -trimpath -ldflags "$(LDFLAGS)" $(CMD)

.PHONY: test
test: ## Run all tests (no network or database required)
	go test ./...

.PHONY: test-race
test-race: ## Run tests with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests and open a coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: test-integration
test-integration: ## Run tests including the Postgres integration suite
	TEST_DATABASE_URL="$(DATABASE_URL)" go test ./... -count=1

.PHONY: test-live
test-live: ## Run the read-only smoke tests against live Stellar testnet
	SOROFORGE_LIVE=1 go test ./internal/stellar/ -run Live -v -count=1

.PHONY: test-e2e
test-e2e: ## Run the deploy/upgrade end-to-end test against a local quickstart network (see internal/e2e/README.md)
	go test -tags e2e ./internal/e2e/ -count=1 -v

.PHONY: fmt
fmt: ## Format the code
	gofmt -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint if it is installed
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run \
		|| echo "golangci-lint not installed; see https://golangci-lint.run/welcome/install/"

.PHONY: check
check: fmt vet test ## Format, vet, and test — run before opening a PR

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: up
up: ## Start Postgres via docker compose
	docker compose up -d

.PHONY: down
down: ## Stop Postgres
	docker compose down

.PHONY: reset-db
reset-db: ## Destroy the Postgres volume and start fresh (deletes all history)
	docker compose down -v
	docker compose up -d

.PHONY: migrate-up
migrate-up: build ## Apply pending migrations
	DATABASE_URL="$(DATABASE_URL)" $(BIN_DIR)/$(BINARY) migrate up

.PHONY: migrate-down
migrate-down: build ## Revert the most recent migration
	DATABASE_URL="$(DATABASE_URL)" $(BIN_DIR)/$(BINARY) migrate down

.PHONY: migrate-version
migrate-version: build ## Show the current schema version
	DATABASE_URL="$(DATABASE_URL)" $(BIN_DIR)/$(BINARY) migrate version

.PHONY: docker-build
docker-build: ## Build the Docker image
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) -t $(BINARY):latest .

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) coverage.out
