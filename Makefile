# Common developer tasks. Everything assumes Go 1.24+ and Node 20+.

SHELL := /bin/bash
IMAGE ?= health-checker-router
TAG   ?= dev
DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/health_checker?sslmode=disable

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Resolve Go dependencies and write go.sum
	go mod tidy

.PHONY: build
build: ## Build the API binary
	go build -o bin/api ./cmd/api

.PHONY: test
test: ## Run Go tests
	go test ./... -race -count=1

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format Go sources
	gofmt -w ./cmd ./internal

.PHONY: lint
lint: vet ## Alias for vet (add golangci-lint here if you adopt it)

.PHONY: web-install
web-install: ## Install frontend dependencies
	cd web && npm install

.PHONY: web-dev
web-dev: ## Run the Vite dev server (proxies /api to :8080)
	cd web && npm run dev

.PHONY: web-build
web-build: ## Build the production frontend bundle
	cd web && npm run build

.PHONY: migrate
migrate: ## Apply database migrations
	goose -dir db/migrations postgres "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	goose -dir db/migrations postgres "$(DATABASE_URL)" down

.PHONY: db-up
db-up: ## Start a local Postgres in Docker
	docker compose up -d postgres

.PHONY: run
run: ## Run the API locally with auth disabled
	AUTH_DISABLED=true DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api

.PHONY: docker
docker: ## Build the container image
	docker build -t $(IMAGE):$(TAG) .

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint deploy/helm

.PHONY: helm-template
helm-template: ## Render the chart to stdout
	helm template health-checker deploy/helm

.PHONY: check
check: vet test web-build ## Everything CI runs
