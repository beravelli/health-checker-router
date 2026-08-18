# Common developer tasks. Everything assumes Go 1.25+ and Node 20+.

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

.PHONY: helm-sync-migrations
helm-sync-migrations: ## Copy db/migrations into the chart (Helm cannot read outside it)
	@mkdir -p deploy/helm/migrations
	@rm -f deploy/helm/migrations/*.sql
	@cp db/migrations/*.sql deploy/helm/migrations/
	@echo "synced $$(ls -1 deploy/helm/migrations/*.sql | wc -l | tr -d ' ') migrations into the chart"

.PHONY: helm-check-migrations
helm-check-migrations: ## Fail if the chart's migration copy has drifted
	@diff -r db/migrations deploy/helm/migrations >/dev/null 2>&1 || { \
		echo "ERROR: deploy/helm/migrations differs from db/migrations."; \
		echo "Run 'make helm-sync-migrations' and commit the result."; \
		diff -r db/migrations deploy/helm/migrations || true; \
		exit 1; }
	@echo "chart migrations match db/migrations"

.PHONY: helm-lint
helm-lint: helm-check-migrations ## Lint the Helm chart
	helm lint deploy/helm --values deploy/helm/values-local.yaml

.PHONY: helm-template
helm-template: helm-check-migrations ## Render the chart to stdout
	helm template health-checker deploy/helm --values deploy/helm/values-local.yaml

.PHONY: local-up
local-up: docker helm-sync-migrations ## Build the image and install the chart on the local cluster
	helm upgrade --install health-checker deploy/helm \
		--namespace health-checker --create-namespace \
		--values deploy/helm/values-local.yaml \
		--set image.tag=$(TAG) \
		--wait --timeout 5m

.PHONY: local-down
local-down: ## Remove the local release and its namespace
	-helm uninstall health-checker --namespace health-checker
	-kubectl delete namespace health-checker

.PHONY: check
check: vet test helm-check-migrations web-build ## Everything CI runs
