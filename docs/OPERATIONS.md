# Operations

Running the gateway locally, in Docker, and on Kubernetes. Back to the [README](../README.md).

## Option A: Docker (full stack)

No Docker on your Mac? `make runtime-up` installs and starts Colima plus the Docker CLI on demand, and `make runtime-down` removes the VM, images, packages and config again. Use `make runtime-up-k8s` to add Kubernetes (k3s) and ingress-nginx.

```bash
docker compose up --build
# Dashboard   http://localhost:3000
# Gateway     http://localhost:8080   (OpenAI-compatible)
# Prometheus  http://localhost:9090
```

Compose starts `pgvector/pgvector:pg16`, Redis 7, MockLLM, the gateway, Prometheus, and the web UI, using `configs/gateway.mock.yaml` so no models are needed. To serve your local Ollama models from the containerized gateway instead, start it with `LLMGW_GATEWAY_CONFIG=/etc/llm-gateway/gateway.yaml`; the gateway reaches Ollama at `http://host.docker.internal:11434` (`LLMGW_OLLAMA_URL`).

Verify the running stack end to end:

```bash
make compose-test          # scripts/compose-smoke.sh: 22 checks (auth, cache tiers, pgvector, guardrails, failover, SSE, 429s, Prometheus, UI)
```

## Option B: Self-contained `.venv` toolchain (no Docker, no system installs)

All tooling lives in `.venv`: Go 1.27, PostgreSQL 16 + pgvector, Redis 7.4, k6, kubectl, the npm cache, and Playwright browsers. Source code stays in the repo. Run `make help` to list every target.

```bash
make venv          # bootstrap the toolchain into .venv, install web deps + Chromium
make test          # Go (-race, real Redis + pgvector) + TypeScript + Jest
make e2e           # Playwright E2E
make bench         # real benchmark -> benchmarks/results.json
make run           # local Postgres + Redis, gateway :8080 on local Ollama models
make run-mock      # same, but with the bundled mock upstream (no model downloads)
make ollama-check  # live integration test through local Ollama
make web-dev       # dashboard on :3000 (in another terminal)
```

## Try it

```bash
TOKEN=$(curl -s -XPOST localhost:8080/v1/auth/token -d '{"tenant_id":"demo"}' | jq -r .access_token)

curl -si localhost:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"qwen3.5:9b","messages":[{"role":"user","content":"Explain how Kafka consumer groups work in simple terms."}]}' | grep -i x-gateway

# Paraphrase -> X-Gateway-Cache: HIT-SEMANTIC (pgvector)
curl -si localhost:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"qwen3.5:9b","messages":[{"role":"user","content":"Can you explain in simple terms how Kafka consumer groups work?"}]}' | grep -i x-gateway-cache

# Failover drill: Qwen fails, Gemma answers (chaos headers are dev-only; also: timeout-primary)
curl -si localhost:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" -H "X-Gateway-Chaos: fail-primary" \
  -d '{"model":"qwen3.5:9b","messages":[{"role":"user","content":"hi"}]}' | grep -iE "provider|upstream-model"

# Embeddings through the gateway (nomic-embed-text, 768-d)
curl -s localhost:8080/v1/embeddings -H "Authorization: Bearer $TOKEN" -d '{"input":"hello"}' | jq '.model, (.data[0].embedding | length)'

# API-key tenant configured with BLOCK mode -> HTTP 400 PII_DETECTED
curl -s localhost:8080/v1/chat/completions -H "X-API-Key: gw-demo-key-block" \
  -d '{"model":"qwen3.5:9b","messages":[{"role":"user","content":"My SSN is 123-45-6789"}]}'
```

Streaming works with `"stream": true`, which returns OpenAI-format SSE regardless of the upstream provider's wire format.

## Local models in containers

**Containers.** `LLMGW_OLLAMA_URL` rewrites `http://localhost:11434` in the config, e.g. `http://host.docker.internal:11434` for a gateway in Docker (see `.env.example`). Docker Compose, CI, the Kubernetes overlay and the benchmark use `configs/gateway.mock.yaml`, so they run without model downloads.

## Kubernetes

`deploy/k8s/base/` contains the production Kustomization: a Namespace, ConfigMap, a Secret example, the gateway Deployment (3 replicas, zone spread, non-root, read-only root FS, `GOMEMLIMIT`/`GOMAXPROCS` from limits, readiness via `/readyz`), Service, PodDisruptionBudget, **HPA** (CPU-based, with a commented `llmgateway_active_connections` custom metric), **Ingress** (ingress-nginx, proxy buffering off for SSE), Redis, and the web UI.

`deploy/k8s/overlays/local/` runs those exact manifests on a single-node cluster. It adds Postgres (pgvector) and MockLLM, uses locally built images, and supplies dev secrets:

```bash
make runtime-up-k8s    # macOS: Colima + k3s + ingress-nginx (or use kind / Docker Desktop + ingress-nginx)
make k8s-deploy        # build images (shared with the cluster runtime) + kubectl apply -k deploy/k8s/overlays/local
make k8s-test          # 19 checks incl. self-healing and zero-downtime rollout
make runtime-down      # remove the VM, images, packages and config
```

CI (`.github/workflows/ci.yml`) runs gofmt, vet, race tests against pgvector and Redis service containers, the 1 ms guardrail latency budget, a benchmark smoke run, typecheck, Jest, the Next.js build, Playwright, Docker image builds, a full `docker compose up` with the end-to-end smoke test, and a **kind** cluster deployment running `scripts/k8s-smoke.sh`.
