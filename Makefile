# LLM Gateway, Cache and Guardrails — developer workflow.
# All tooling lives in .venv (see scripts/bootstrap-venv.sh); every target
# runs with it activated.
SHELL := /bin/bash
VENV  := source .venv/bin/activate &&
PG_DSN ?= postgres://llmgw@localhost:5432/llmgateway?sslmode=disable
REDIS_ADDR ?= localhost:6379
TEST_ENV := LLMGW_TEST_REDIS_ADDR=$(REDIS_ADDR) LLMGW_TEST_POSTGRES_DSN="$(PG_DSN)"

.PHONY: help venv services services-down build test test-go test-web e2e e2e-live bench bench-quick k6 run up down compose-test k8s-deploy k8s-test web-dev fmt vet clean

help: ## Show targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

venv: ## Bootstrap the self-contained toolchain into .venv
	scripts/bootstrap-venv.sh
	$(VENV) cd web && npm ci --no-audit --no-fund && npx playwright install chromium

services: ## Start local Postgres(pgvector) + Redis from .venv
	scripts/dev-services.sh start

services-down: ## Stop local Postgres + Redis
	scripts/dev-services.sh stop

build: ## Build Go binaries into ./bin and the web app
	$(VENV) go build -trimpath -o bin/ ./cmd/...
	$(VENV) cd web && npm run build

vet: ## go vet + gofmt check
	$(VENV) go vet ./... && test -z "$$(gofmt -l cmd internal migrations)"

fmt: ## Format Go code
	$(VENV) gofmt -w cmd internal migrations

test: test-go test-web ## Run all unit/integration tests

test-go: services ## Go tests with the race detector against real Redis + pgvector
	$(VENV) $(TEST_ENV) go test -race -count=1 ./...

test-web: ## Jest unit/component tests + typecheck
	$(VENV) cd web && npx tsc --noEmit && npm test

e2e: ## Playwright E2E (mocked gateway)
	$(VENV) cd web && npm run build && CI=1 npx playwright test

e2e-live: ## Playwright E2E against a running gateway on :8080 (make run)
	$(VENV) cd web && npm run build && CI=1 E2E_LIVE=1 npx playwright test

bench: services ## Real benchmark (5,000 req x 100 workers) -> benchmarks/results.json
	$(VENV) go run ./cmd/bench -requests 5000 -concurrency 100 -redis $(REDIS_ADDR) -postgres "$(PG_DSN)" -out benchmarks/results.json

bench-quick: services ## Smaller benchmark run (not written to results.json)
	$(VENV) go run ./cmd/bench -requests 1000 -concurrency 50 -hot-requests 2000 -out /tmp/llmgw-bench-quick.json

k6: ## k6 load test against a running gateway (make run)
	$(VENV) k6 run -e DURATION=30s benchmarks/k6-load-test.js

run: services build ## Run mockllm + gateway locally (Ctrl-C to stop)
	@trap 'kill 0' INT TERM; \
	./bin/mockllm & \
	LLMGW_CORS_ORIGINS="http://localhost:3000,http://127.0.0.1:3000,http://127.0.0.1:3100" ./bin/gateway -config configs/gateway.yaml & \
	wait

web-dev: ## Next.js dev server on :3000
	$(VENV) cd web && npm run dev

up: ## docker compose up --build (full stack incl. Prometheus + UI)
	docker compose up --build

down: ## docker compose down -v
	docker compose down -v

compose-test: ## End-to-end smoke test of the running compose stack
	scripts/compose-smoke.sh

k8s-deploy: ## Build images and deploy to the current kube context (local overlay)
	docker compose build gateway mockllm web
	$(VENV) kubectl apply -k deploy/k8s/overlays/local

k8s-test: ## Kubernetes end-to-end test (self-healing, zero-downtime rollout, HPA, PDB)
	$(VENV) scripts/k8s-smoke.sh

clean: ## Remove build outputs and local service data
	rm -rf bin web/.next web/test-results web/playwright-report .data
