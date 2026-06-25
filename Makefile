# ============================================================
# chess-server Makefile
# ============================================================
# Prerequisites:
#   - Go 1.22+
#   - Docker and docker-compose
#   - migrate CLI  (install with: make install-tools)
#   - golangci-lint (install with: make install-tools)
#
# First-time setup:
#   cp .env.example .env          # fill in real values
#   make docker-up                # start PostgreSQL
#   make migrate-up               # apply all migrations
#   make run                      # start the server
# ============================================================

# Load .env if it exists — exports all variables into make's environment.
# The leading dash means "don't fail if .env is missing".
-include .env
export

# Binary output path
BINARY := bin/server

# Go build flags
BUILD_FLAGS := -trimpath

# Tool versions (pin these to match go.mod)
MIGRATE_VERSION  := v4.18.1
GOLANGCI_VERSION := v1.62.2

.PHONY: help run build test test-race test-integration \
        migrate-up migrate-down \
        docker-up docker-down \
        lint vet tidy install-tools clean

# ---- Default target ----------------------------------------

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ---- Development -------------------------------------------

run: ## Run the server (requires .env)
	go run ./cmd/server/...

build: ## Build the server binary to ./bin/server
	go build $(BUILD_FLAGS) -o $(BINARY) ./cmd/server/...

# ---- Testing -----------------------------------------------

test: ## Run unit tests (no database required)
	go test ./...

test-race: ## Run unit tests with the race detector (required before every commit)
	go test -race ./...

test-integration: ## Run integration tests (requires DATABASE_URL)
	go test -race -tags integration ./...

# ---- Database migrations -----------------------------------
# Requires: migrate CLI (run 'make install-tools' first)
# Requires: DATABASE_URL set in environment or .env

migrate-up: ## Apply all pending migrations
	migrate -path ./migrations -database "$(DATABASE_URL)" up

migrate-down: ## Roll back exactly one migration
	migrate -path ./migrations -database "$(DATABASE_URL)" down 1

migrate-drop: ## Drop everything in the database — DESTRUCTIVE, dev only
	migrate -path ./migrations -database "$(DATABASE_URL)" drop -f

# ---- Docker ------------------------------------------------

docker-up: ## Start PostgreSQL in Docker (detached)
	docker compose up -d postgres

docker-down: ## Stop and remove Docker containers (data volume preserved)
	docker compose down

docker-start: ## Start docker containers
	docker compose start

docker-stop: ## Stop Docker containers (data volume and container preserved)
	docker compose stop

docker-reset: ## Stop containers AND remove the data volume — DESTRUCTIVE
	docker compose down -v

# ---- Code quality ------------------------------------------

lint: ## Run golangci-lint
	golangci-lint run ./...

vet: ## Run go vet
	go vet ./...

tidy: ## Tidy and verify go.mod / go.sum
	go mod tidy
	go mod verify

# ---- Tooling -----------------------------------------------

install-tools: ## Install migrate CLI and golangci-lint
	@echo "Installing migrate CLI $(MIGRATE_VERSION)..."
	go install github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)
	@echo "Installing golangci-lint $(GOLANGCI_VERSION)..."
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_VERSION)
	@echo "Done. Make sure $(shell go env GOPATH)/bin is in your PATH."

# ---- Cleanup -----------------------------------------------

clean: ## Remove build artifacts
	rm -rf bin/
