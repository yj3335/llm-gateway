# Testing

What is verified, and how. Back to the [README](../README.md).

## Test suites

| Suite | Count | What it covers |
|---|---:|---|
| Go unit + integration (`go test -race ./...`) | **120 passed (86 test functions), 0 failed, 0 skipped, 0 data races** | JWT HS256/RS256/JWKS and `alg` confusion; layered key store; Lua dual bucket against **real Redis** (exact admission under 300 concurrent goroutines, TPM atomicity, refill, fallback and recovery); PII and injection tables with false-positive cases, a streaming redactor at multiple chunk sizes, and a **p99 latency budget test**; embedder separability; **live pgvector HNSW** search; breaker state machine (stale tickets, fast-failure reordering); every provider wire format over HTTP; 5xx/timeout/open-circuit failover; full-pipeline gateway tests (cache tiers, coalescing, 429s, PII, output redaction, SSE, CORS, telemetry); startup dependency retry; Ollama and fallback embedders, embedder separation, mixed-dimension pgvector, schema migration and upgrade, `extra_body`, injected faults, `/v1/embeddings`; config and Kubernetes ConfigMap validation |
| Live local models (`make ollama-check`) | **9 passed** | Through real Ollama with `configs/gateway.yaml`: chat, streaming, embeddings, neural semantic cache hit and near-miss, 503 and timeout failover to Gemma, breaker trip and recovery, embedder-offline fallback. Runs locally; CI has no Ollama. |
| Jest (`web/__tests__`) | **27 passed** | formatting utilities, playground request planning, SSE parsing, KPI / provider-matrix / audit-table / playground components |
| Playwright (`web/e2e/dashboard.spec.ts`) | **4 passed** (+1 live) | dashboard rendering, SSE→polling fallback, playground toggles (semantic cache, PII, failover, injection), offline banner. The `E2E_LIVE=1` spec drives the real gateway (miss → semantic hit, redaction, failover) and passed against the running stack. |
| `npm run build` | ✓ | zero TypeScript or build errors |
| Docker Compose end-to-end (`scripts/compose-smoke.sh`) | **22 passed** | Full containerized stack (gateway, MockLLM, pgvector, Redis, Prometheus, web UI): auth/401, MISS → HIT-EXACT → HIT-SEMANTIC from containerized pgvector, PII REDACT and BLOCK, injection block, chaos failover, SSE translation, 429 + `Retry-After`, rows in `semantic_cache` / `guardrail_audit`, per-dimension HNSW indexes, `/metrics`, telemetry SSE, Prometheus scraping (`up=1`), dashboard + CORS. The live Playwright spec also passes against the dashboard container. Runs in CI on every push. |
| Kubernetes end-to-end (`scripts/k8s-smoke.sh`) | **19 passed** | The production manifests plus a local overlay on a real cluster (k3s locally, kind in CI) with ingress-nginx and metrics-server: 3/3 replicas ready as non-root UID 65532 with a read-only root FS; all 19 functional checks routed through the **Ingress** and load-balanced across all 3 replicas (shared Redis/pgvector cache); metrics aggregated across pods; HPA reading live CPU; PDB allowing exactly 1 disruption; **a pod deleted under load → 291 requests, 0 failures**; **rolling restart of all replicas → 590 requests, 0 failures**; Redis outage → requests keep succeeding on the local fallback, then return to Redis automatically. Runs in CI on every push. |

## Live local-model check (`make ollama-check`)

**`make ollama-check` (live, on an Apple Silicon laptop with real Redis and pgvector): 9/9 checks pass in about 12 s.**

| Check | Measured result |
|---|---|
| Chat completion through `qwen3.5:9b` | real answer in ~1.0–1.2 s (warm model) |
| Streaming through `qwen3.5:9b` | OpenAI-format SSE chunks with content |
| `POST /v1/embeddings` | 768-dimension `nomic-embed-text` vector, unit norm |
| Paraphrase served from the neural semantic cache | `HIT-SEMANTIC` through pgvector, similarity 0.973 |
| Near-miss ("TCP congestion control" vs "TCP flow control") | `MISS`: not served a wrong cached answer |
| Injected 503 on Qwen | answered by `gemma4:e4b`, `X-Gateway-Provider-Failover: true` |
| Injected timeout on Qwen | answered by `gemma4:e4b` |
| Circuit breaker | trips OPEN after repeated faults; Qwen short-circuited; recovers OPEN → HALF-OPEN → CLOSED |
| Embedder offline | chat still answered by Qwen; cache and `/v1/embeddings` use the hashing fallback; `/readyz` reports `embedder: degraded` |

## Running the tests

```bash
make test            # Go (race detector, real Redis + pgvector) + TypeScript + Jest
make e2e             # Playwright
make ollama-check    # live test through local Ollama
make compose-test    # against a running docker compose stack
make k8s-test        # against the local Kubernetes overlay
```

CI (`.github/workflows/ci.yml`) runs everything except the Ollama check on every push: Go, web, Docker Compose end to end, and Kubernetes on a kind cluster.
