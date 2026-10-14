# LLM Gateway, Cache and Guardrails

A high-throughput, cloud-native **LLM semantic gateway** written in Go: an OpenAI-compatible proxy that sits in front of OpenAI, Anthropic, Gemini, and Ollama. It provides:

- multi-tenant auth,
- an atomic Redis dual token-bucket rate limiter (RPM + TPM),
- a sub-millisecond Responsible-AI guardrail firewall (PII redaction and prompt-injection blocking),
- a two-tier exact + **pgvector semantic cache**,
- per-provider **circuit breakers with automatic failover**.

A **Next.js + TypeScript control plane** streams live telemetry and includes an interactive playground.

Every number in this README was **measured**. Benchmark figures come from `cmd/bench` and are stored in [`benchmarks/results.json`](benchmarks/results.json). Docker, Kubernetes and k6 figures come from `scripts/compose-smoke.sh`, `scripts/k8s-smoke.sh` and `benchmarks/k6-load-test.js`.

---

## Headline results (measured)

Workload: 5,000 requests across 100 concurrent workers, real Redis 7.4 and PostgreSQL 16 + pgvector 0.6, and a 10% injected upstream 503 rate. See [Benchmarks](#benchmarks) for the full table and methodology.

| Metric | Result |
|---|---|
| Mixed-workload throughput | **1,515 req/s** (100 workers, upstream p50 168 ms) |
| Hot-cache throughput | **29,922 req/s** |
| Gateway overhead (all requests) | **p50 0.98 ms · p95 3.29 ms** · p99 23.0 ms |
| Cache hit ratio | **61.85%**: 35.74% exact + 26.10% semantic |
| Semantic-cache precision | **98.75%** (1,276 semantic hits verified against ground truth) |
| Latency reduction on a cache hit | **99.67%** at p50 (0.56 ms vs 170.6 ms) · **61.5%** lower mean end-to-end latency |
| Automatic failover success | **100%**: 162 injected 503s, 0 unrecovered errors |
| Guardrails | 561/561 PII prompts redacted · 112/112 injections blocked · **0 false positives** |
| Guardrail latency | **2.9 µs** p50 typical prompt · **121 µs** p99 for a 3.6 KB PII-laden prompt |
| Kubernetes availability | Rolling restart of all 3 replicas: **590 requests, 0 failures** · pod killed under load: **0 failures** |

---

## Architecture

```mermaid
flowchart LR
    C([Client / SDK<br/>OpenAI-compatible]) -->|POST /v1/chat/completions| A

    subgraph GW[Go Gateway :8080]
      direction LR
      A[JWT / OAuth2 / API-Key Auth<br/>L1 memory → Redis → Postgres] --> RL
      RL[Redis Lua Dual Token Bucket<br/>RPM + TPM, atomic<br/><i>local fallback if Redis down</i>] --> G
      G[Responsible-AI Guardrail<br/>PII regex + Luhn · injection scoring<br/>REDACT / BLOCK, under 1 ms] --> L1
      L1[L1 Exact Cache<br/>sharded LRU → Redis] -->|miss| L2
      L2[L2 Semantic Cache<br/>384-d embedding → pgvector HNSW<br/>cosine ≥ 0.86] -->|miss| SF
      SF[Request Coalescing<br/>singleflight per prompt] --> R
      R[Circuit-Breaking Multi-Provider Router<br/>CLOSED · OPEN · HALF-OPEN] --> OG
      OG[Output Guardrail<br/>streaming-safe redaction] -.write-behind.-> L1 & L2
    end

    R -->|primary| P1[(OpenAI)]
    R -.failover.-> P2[(Anthropic)]
    R -.failover.-> P3[(Gemini)]
    R -.failover.-> P4[(Ollama)]
    R -.local dev/bench.-> M[(MockLLM :8090)]

    GW --> T[Telemetry<br/>Prometheus /metrics · JSON audit log<br/>/v1/telemetry/summary · SSE stream]
    T --> UI[Next.js Control Plane :3000]
    T --> PR[Prometheus :9090]

    L1 --- RD[(Redis 7)]
    RL --- RD
    L2 --- PG[(PostgreSQL 16<br/>pgvector HNSW)]
    G -.audit.-> PG
```

### Request lifecycle

1. **Auth** (`internal/auth`): accepts `Authorization: Bearer <JWT>` signed with HS256, RS256 with a static PEM key, or RS256 from an OAuth2/OIDC **JWKS** endpoint (with key rotation by `kid`). It also accepts `X-API-Key`, looked up through **L1 memory → Redis → PostgreSQL**. Keys are stored only as SHA-256 hashes, and unknown keys are negatively cached. The JWT claims `tenant_id`, `tier`, `rpm_limit`, `tpm_limit`, and `pii_mode` define the tenant policy.
2. **Rate limit** (`internal/ratelimit`): one **atomic Lua script** refills and checks *both* buckets using the Redis server clock, and debits them only if **both** have capacity. TPM is charged with an estimate (prompt chars/4 + `max_tokens`) and **reconciled** after the response; cache hits are refunded in full. Bucket keys use `{tenant}` hash tags, so they're safe on Redis Cluster. If Redis errors, a sharded in-process bucket takes over and Redis is retried after a backoff.
3. **Guardrails** (`internal/guardrails`): PII detection uses regexes, **Luhn + IIN** validation for cards, SSN area/group/serial rules, emails, phones, AWS keys, and OpenAI/GitHub/Slack/Google API keys. Matches are redacted (`[REDACTED_SSN]`, …) or blocked (`400 PII_DETECTED`) according to the tenant's mode. The injection firewall combines 11 weighted heuristics with a noisy-OR. They run over leetspeak-folded and spaced-letter-collapsed text, plus delimiter and chat-template tokens, role spoofing, and **base64-encoded payloads**, which are decoded and re-scored. Prompts at or above 0.70 are blocked (`400 PROMPT_INJECTION`).
4. **Cache** (`internal/cache`): the **exact** key is `sha256(tenant, model, normalized conversation)`. The **semantic** lookup embeds the final user turn and searches pgvector, scoped by tenant, model, and a **scope hash** of the system prompt and prior turns, so a paraphrase can only match an identical context. Writes are asynchronous (write-behind), and PII is redacted *before* anything is cached.
5. **Coalescing:** concurrent identical misses share one upstream call through `singleflight`. In tests, 25 concurrent identical requests produced exactly 1 upstream call.
6. **Router** (`internal/router`): adapters translate OpenAI ⇄ Anthropic Messages ⇄ Gemini `generateContent`, for both JSON and SSE. A `5xx`, `429`, timeout, or transport error fails over to the next target; other `4xx` errors don't and never trip the breaker. Streams fail over before the first byte is relayed.
7. **Output guardrails:** JSON responses are redacted directly. Streams go through a holdback-window redactor that catches PII split across chunk boundaries, with tests at 1-, 3-, 7-, 16-, and 64-byte chunk sizes.

### Response headers

| Header | Meaning |
|---|---|
| `X-Gateway-Cache` | `HIT-EXACT` · `HIT-SEMANTIC` · `MISS` · `BYPASS` |
| `X-Gateway-Cache-Similarity`, `X-Gateway-Cache-Layer` | cosine score; `memory` / `redis` / `pgvector` |
| `X-Gateway-Provider`, `X-Gateway-Provider-Failover` | serving provider; `true` if a fallback served the request |
| `X-Gateway-Overhead-Ms`, `X-Gateway-Upstream-Latency-Ms`, `X-Gateway-Total-Latency-Ms` | latency breakdown |
| `X-Gateway-Guardrail`, `X-Gateway-Guardrail-Categories` | `REDACT` / `BLOCK` / `FLAG`; e.g. `CREDIT_CARD,SSN` |
| `X-RateLimit-Limit-Requests`, `X-RateLimit-Remaining-Requests`, `X-RateLimit-Limit-Tokens`, `X-RateLimit-Remaining-Tokens`, `X-RateLimit-Reset`, `Retry-After` | standard quota headers (429 on exhaustion) |
| `X-Gateway-Tokens-Saved`, `X-Gateway-Coalesced`, `X-Request-ID` | savings, coalescing, tracing |

---

## Key design decisions

- **Rate-based circuit breaking in admission order.** The breaker trips when ≥50% of the last 20 calls failed (minimum 10), and counts each result only after every earlier-admitted call has resolved, so fast failures can't masquerade as a burst. See issue 1 below.
- **Pre-warmed connection pool and batched hit counters.** Bursts never pay connection setup in the request path, and hot cache rows aren't updated once per hit. See issue 2 below.
- **Deterministic local embedder behind an `Embedder` interface.** It uses signed feature hashing of stemmed, synonym-canonicalized unigrams, bigrams, and character trigrams into 384 dimensions (2 µs per prompt), so benchmarks need no external API. It captures *lexical* paraphrases such as reordering, contractions, filler words, and common synonyms, not deep semantics. For production, set `cache.embedder: openai` (an OpenAI-compatible `/v1/embeddings` endpoint called with `dimensions=384`); the pgvector schema stays the same. Precision is **measured** rather than assumed: the mock echoes the prompt it answered, so the benchmark verifies every semantic hit against ground truth.
- **Fail open on infrastructure, fail closed on policy.** If Redis or Postgres is down, the gateway degrades to local rate limiting and an in-memory cache and key store rather than going offline. Guardrails never fail open.
- **Overhead is measured, not estimated.** The gateway reports total time minus upstream time per request. Coalesced followers subtract their actual wait.

## Issues found during development

Every issue below was found by a measurement, a test, or a real deployment, not by inspection. Each row lists what we saw, the root cause, and the fix.

| # | Area | Symptom | Root cause | Fix |
|---|---|---|---|---|
| 1 | Circuit breaker | 5 injected faults caused 320 failovers; the primary was effectively switched off. | Instant 503s finished ahead of ~170 ms successes, so at 100 concurrent requests the breaker saw a burst of "consecutive" failures at a true 10% error rate. | Outcomes enter the window in **admission order**, and the breaker trips on ≥50% failures over the last 20 calls (minimum 10). Regression test `TestBreakerNoFalseTripOnFastFailures`. |
| 2 | Semantic cache | Cache-miss lookups had a 47 ms p95 tail. | Not lock contention (batching hit counters didn't help). The pgx pool opened connections lazily, ~50 ms each with pgvector type registration, during bursts. | Pre-warm `min_conns` at startup: p95 47 ms → 4.6 ms. Hit counters also batched into one `UPDATE … FROM unnest()`. |
| 3 | Guardrails | 1.2 ms per 3.6 KB prompt, over the 1 ms budget. | RE2 regexes scanned the whole prompt, several times. | Literal keyword gates, and PII regexes run only in windows around digit clusters and `@`: 2.9 µs typical, 121 µs p99 at 3.6 KB. |
| 4 | Kubernetes | Every gateway and web pod stuck in `CreateContainerConfigError`. | Images declared `USER nonroot` / `USER node`; with `runAsNonRoot: true` the kubelet can't verify a *named* user. Docker Compose doesn't check this, so compose tests had passed. | Numeric UIDs in both images (65532, 1000), and `runAsUser` / `runAsGroup` pinned in the pod specs. |
| 5 | Startup | Possible silent degradation: if the gateway started before Postgres, it ran on in-memory fallbacks forever. | Containers start in any order in Kubernetes and Compose. | `server.dependency_wait` (default 30 s) retries Redis and Postgres before falling back; unit-tested. |
| 6 | Telemetry | Behind the load balancer, `/metrics` and `/v1/telemetry/summary` missed requests. | Each replica counts only its own traffic. | The Kubernetes test aggregates metrics across pods, as Prometheus does. Per-replica dashboard data is listed under Limitations. |
| 7 | Kubernetes test (CI) | Intermittent failure: "Ingress routes to the gateway Service", while every later check through the same Ingress passed. | ingress-nginx answers 404/503 for a few seconds after an Ingress is applied, and the wait loop used `curl -s`, which succeeds on *any* HTTP status. | Wait until the Ingress returns a real 200 (up to 2 min). |
| 8 | Test scripts | Checks failed even though `grep` matched; the SSE check failed against a working stream. | Under `set -o pipefail`, `grep -q` exits early and `kubectl logs` gets SIGPIPE; and `curl --max-time` on an endless SSE stream always exits 28. Both turn a pipeline "red" despite a match. | Read the full input (`grep … >/dev/null`), and capture SSE to a file before matching. |
| 9 | CI timing | Tests that passed locally were at risk on 2-vCPU runners: exact token-bucket admission, and the 1 ms guardrail budget under `-race`. | Token refill during a slow test, and race-detector slowdown (5–20×). | The admission test allows for elapsed-time refill (still catches lost or double-spent tokens). The latency budget is enforced in a separate non-race CI step. |
| 10 | Spin-up script | `make runtime-up-k8s` failed with "kubectl not found" from a normal terminal. | Colima checks for `kubectl` before starting k3s; `kubectl` lives in `.venv`, and the manual test had the venv active. | The script activates `.venv` first; verified by a clean-terminal round-trip (install → deploy → 19 checks → remove). |
| 11 | Kubernetes manifests | `kubectl apply -k` failed with "cycle detected". | The local overlay lived inside its own base directory. | Standard layout: `deploy/k8s/base` plus `deploy/k8s/overlays/local`. |
| 12 | Local cluster | The production Ingress had no controller. | Colima's k3s disables Traefik and ships no Ingress controller. | Install the official ingress-nginx, the class the manifest targets (automated in `make runtime-up-k8s` and CI). |
| 13 | Browser tests | Playwright tests crashed on click ("GPU process isn't usable") on recent macOS. | The stripped-down Chromium headless shell fails there. | Playwright uses full Chromium in new-headless mode with `--disable-gpu`. |
| 14 | Docker build | Risk: a lower Go base image (for example 1.24) would fail at `go mod download`. | go-redis and `golang.org/x/*` require Go ≥ 1.26, and `go.mod` declares 1.27.2. | Builder pinned to `golang:1.27.2-alpine`, matching `go.mod` exactly. All base-image tags were verified against their registries. |

---

## Benchmarks

`make bench` runs `cmd/bench`, which starts **mockllm and the gateway in-process on real loopback TCP ports**. It connects to the real **Redis 7.4** and **PostgreSQL 16 + pgvector 0.6** from `.venv`, runs four phases, and writes `benchmarks/results.json`.

**Environment:** Apple Silicon arm64, 15 CPUs, macOS, Go 1.27.2. The load generator, gateway, mock upstream, Redis, and Postgres all share one machine, which inflates tail latency.
**Mock upstream:** TTFT 120 ms ±15% jitter, 1 ms per token, 48 tokens, giving a realistic ~168 ms uncached latency.
**Workload:** 300 head intents (60 topics × 5 question types) × 4 human-style paraphrases, chosen by **zipf (s = 1.1)**, plus 25% never-repeating long-tail prompts, 10% PII prompts, 2% prompt-injection attempts, and a **10% injected 503 rate** on the primary provider (OpenAI-format), failing over to Anthropic, then Gemini.

### Results (`benchmarks/results.json`)

| Phase / metric | p50 | p95 | p99 | Notes |
|---|---:|---:|---:|---|
| **Uncached upstream baseline** (direct to mock, 500 req) | 168.15 ms | 184.75 ms | 186.72 ms | no gateway |
| **Gateway overhead**, all 4,888 successful requests | **0.979 ms** | **3.294 ms** | 23.018 ms | mean 1.50 ms |
| Gateway overhead, cache hits (3,023) | 0.251 ms | 2.835 ms | 3.637 ms | |
| Gateway overhead, cache misses (1,865) | 1.325 ms | 16.443 ms | 25.848 ms | includes the pgvector probe |
| Client latency, cache hit | **0.559 ms** | 3.090 ms | 3.933 ms | |
| Client latency, cache miss | 170.641 ms | 187.630 ms | 205.768 ms | |
| Upstream latency seen by the gateway (misses) | 168.021 ms | 184.236 ms | 185.781 ms | |
| Cache lookup, misses (memory → Redis → pgvector) | 1.127 ms | 4.583 ms | 13.485 ms | |
| **Hot-cache phase** (20,000 req, 100% hits) | 1.413 ms overhead | 2.190 ms | 2.576 ms | **29,921.6 req/s** |
| Guardrails, typical prompt (20k runs) | 2.9 µs | 8.5 µs | 10.6 µs | in-process |
| Guardrails, 3.6 KB prompt with PII (2k runs) | 85.4 µs | 101.2 µs | 121.4 µs | |
| Embedding (hashing, 384-d) | 2.0 µs | 2.6 µs | 4.9 µs | |

| Effectiveness metric | Value |
|---|---|
| Mixed-workload throughput | **1,515.1 req/s** (5,000 requests in 3.30 s, 100 workers) |
| Cache hit ratio, overall | **61.85%** (exact 35.74%, semantic 26.10%) |
| Hit ratio on repeated-intent traffic (excludes the unique long tail) | 82.73% |
| Semantic precision (hit returned the *same intent's* answer) | **98.75%** of 1,276 verified semantic hits |
| Average semantic similarity | 0.974 |
| p50 latency reduction, hit vs. miss | **99.67%** |
| Mean end-to-end latency reduction vs. all-miss | **61.47%** |
| Upstream calls avoided (hits + coalesced) | 3,098 of 5,000 requests (75 coalesced) |
| Tokens saved / estimated spend saved | 224,378 tokens / $1.65 (at $2.50/$10 per 1M in/out) |
| Injected upstream 503s | 162 of 1,984 upstream calls |
| Failover responses / unrecovered 5xx | 172 / **0**, so **100% failover success** (the 10 extra responses are coalesced followers sharing a failed-over result) |
| PII prompts redacted | 561 / 561 |
| Prompt injections blocked | 112 / 112, with **0** false-positive blocks on 4,888 benign requests |
| HTTP status mix | 4,888 × 200 · 112 × 400 (all intended injection blocks) |

**Interpretation:** the p99 overhead on misses (~25 ms) is Postgres HNSW search competing for CPU with the load generator and 100 in-flight handlers on one laptop. Hits stay under 4 ms at p99. The embedder's offline separability check found a minimum paraphrase similarity of 0.63 and a maximum cross-intent similarity of 0.82, against a 0.86 threshold. Some paraphrases therefore *miss* the semantic tier (by design, favoring precision) and are answered upstream.

**k6** (`benchmarks/k6-load-test.js`, `make k6`): 10-second runs, all thresholds passing (100% failover success, every injection blocked):

| Target | Throughput | Gateway overhead p95 | Client latency p95 |
|---|---:|---:|---:|
| Native gateway processes (15 CPUs) | 6,863 req/s | 0.75 ms | 1.38 ms |
| `docker compose` stack in a 2-CPU / 4 GB Linux VM | 3,310 req/s | 1.20 ms | 3.38 ms |

Under that load the whole compose stack used about 220 MB of RAM (gateway 32 MB, Postgres 126 MB).

---

## Quickstart

### Option A: Docker (full stack)

No Docker on your Mac? `make runtime-up` installs and starts Colima plus the Docker CLI on demand, and `make runtime-down` removes the VM, images, packages and config again. Use `make runtime-up-k8s` to add Kubernetes (k3s) and ingress-nginx.

```bash
docker compose up --build
# Dashboard   http://localhost:3000
# Gateway     http://localhost:8080   (OpenAI-compatible)
# Prometheus  http://localhost:9090
```

Compose starts `pgvector/pgvector:pg16`, Redis 7, MockLLM, the gateway, Prometheus, and the web UI. Set `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, and `GEMINI_API_KEY`, and point the `providers[].base_url` values in `configs/gateway.yaml` at real endpoints to use live models.

Verify the running stack end to end:

```bash
make compose-test          # scripts/compose-smoke.sh: 22 checks (auth, cache tiers, pgvector, guardrails, failover, SSE, 429s, Prometheus, UI)
```

### Option B: Self-contained `.venv` toolchain (no Docker, no system installs)

All tooling lives in `.venv`: Go 1.27, PostgreSQL 16 + pgvector, Redis 7.4, k6, kubectl, the npm cache, and Playwright browsers. Source code stays in the repo. Run `make help` to list every target.

```bash
make venv          # bootstrap the toolchain into .venv, install web deps + Chromium
make test          # Go (-race, real Redis + pgvector) + TypeScript + Jest
make e2e           # Playwright E2E
make bench         # real benchmark -> benchmarks/results.json
make run           # local Postgres + Redis, mockllm :8090, gateway :8080
make web-dev       # dashboard on :3000 (in another terminal)
```

### Try it

```bash
TOKEN=$(curl -s -XPOST localhost:8080/v1/auth/token -d '{"tenant_id":"demo"}' | jq -r .access_token)

curl -si localhost:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Explain how Kafka consumer groups work in simple terms."}]}' | grep -i x-gateway

# Paraphrase -> X-Gateway-Cache: HIT-SEMANTIC (pgvector)
curl -si localhost:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Can you explain in simple terms how Kafka consumer groups work?"}]}' | grep -i x-gateway-cache

# Failover drill (chaos headers are dev-only)
curl -si localhost:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" -H "X-Gateway-Chaos: fail-primary" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | grep -i provider

# API-key tenant configured with BLOCK mode -> HTTP 400 PII_DETECTED
curl -s localhost:8080/v1/chat/completions -H "X-API-Key: gw-demo-key-block" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"My SSN is 123-45-6789"}]}'
```

Streaming works with `"stream": true`, which returns OpenAI-format SSE regardless of the upstream provider's wire format.

---

## Testing

| Suite | Count | What it covers |
|---|---:|---|
| Go unit + integration (`go test -race ./...`) | **106 passed, 0 failed, 0 skipped, 0 data races** | JWT HS256/RS256/JWKS and `alg` confusion; layered key store; Lua dual bucket against **real Redis** (exact admission under 300 concurrent goroutines, TPM atomicity, refill, fallback and recovery); PII and injection tables with false-positive cases, a streaming redactor at multiple chunk sizes, and a **p99 latency budget test**; embedder separability; **live pgvector HNSW** search; breaker state machine (stale tickets, fast-failure reordering); every provider wire format over HTTP; 5xx/timeout/open-circuit failover; full-pipeline gateway tests (cache tiers, coalescing, 429s, PII, output redaction, SSE, CORS, telemetry); startup dependency retry; config and Kubernetes ConfigMap validation |
| Jest (`web/__tests__`) | **27 passed** | formatting utilities, playground request planning, SSE parsing, KPI / provider-matrix / audit-table / playground components |
| Playwright (`web/e2e/dashboard.spec.ts`) | **4 passed** (+1 live) | dashboard rendering, SSE→polling fallback, playground toggles (semantic cache, PII, failover, injection), offline banner. The `E2E_LIVE=1` spec drives the real gateway (miss → semantic hit, redaction, failover) and passed against the running stack. |
| `npm run build` | ✓ | zero TypeScript or build errors |
| Docker Compose end-to-end (`scripts/compose-smoke.sh`) | **22 passed** | Full containerized stack (gateway, MockLLM, pgvector, Redis, Prometheus, web UI): auth/401, MISS → HIT-EXACT → HIT-SEMANTIC from containerized pgvector, PII REDACT and BLOCK, injection block, chaos failover, SSE translation, 429 + `Retry-After`, rows in `semantic_cache` / `guardrail_audit`, HNSW index, `/metrics`, telemetry SSE, Prometheus scraping (`up=1`), dashboard + CORS. The live Playwright spec also passes against the dashboard container. Runs in CI on every push. |
| Kubernetes end-to-end (`scripts/k8s-smoke.sh`) | **19 passed** | The production manifests plus a local overlay on a real cluster (k3s locally, kind in CI) with ingress-nginx and metrics-server: 3/3 replicas ready as non-root UID 65532 with a read-only root FS; all 19 functional checks routed through the **Ingress** and load-balanced across all 3 replicas (shared Redis/pgvector cache); metrics aggregated across pods; HPA reading live CPU; PDB allowing exactly 1 disruption; **a pod deleted under load → 291 requests, 0 failures**; **rolling restart of all replicas → 590 requests, 0 failures**; Redis outage → requests keep succeeding on the local fallback, then return to Redis automatically. Runs in CI on every push. |

---

## Observability

- **`/metrics`** (Prometheus): `llmgateway_requests_total{model,cache,code}`, `llmgateway_gateway_overhead_seconds`, `llmgateway_upstream_latency_seconds{provider}`, `llmgateway_cache_lookups_total{result}`, `llmgateway_cache_similarity`, `llmgateway_tokens_saved_total`, `llmgateway_cost_saved_usd_total`, **`llmgateway_guardrail_triggers_total{type,action}`**, `llmgateway_ratelimit_rejections_total{reason}`, `llmgateway_circuit_breaker_state{provider}`, `llmgateway_provider_failovers_total{from,to}`, and more.
- **`/v1/telemetry/summary`** and **`/v1/telemetry/stream`** (SSE, 1 s): totals, RPS, p50/p95/p99 overhead vs. upstream, exact/semantic/miss counts, USD saved, guardrail counts by category, breaker state and latency per provider, and the 50 most recent request traces.
- **Audit:** one structured JSON line per request and guardrail event. Guardrail events are also batch-`COPY`'d into the `guardrail_audit` table.

## Configuration

`configs/gateway.yaml` is the documented default, and every value can be overridden with environment variables (`LLMGW_REDIS_ADDR`, `LLMGW_POSTGRES_DSN`, `LLMGW_JWT_SECRET`, `LLMGW_JWKS_URL`, `LLMGW_CORS_ORIGINS`, `LLMGW_MOCKLLM_URL`, …). `server.dependency_wait` (default 30s) sets how long the gateway retries Redis and PostgreSQL at startup before falling back to in-memory mode, since containers can start in any order. The production Kubernetes ConfigMap disables the dev token endpoint and chaos headers, and a test enforces this.

## Deployment

`deploy/k8s/base/` contains the production Kustomization: a Namespace, ConfigMap, a Secret example, the gateway Deployment (3 replicas, zone spread, non-root, read-only root FS, `GOMEMLIMIT`/`GOMAXPROCS` from limits, readiness via `/readyz`), Service, PodDisruptionBudget, **HPA** (CPU-based, with a commented `llmgateway_active_connections` custom metric), **Ingress** (ingress-nginx, proxy buffering off for SSE), Redis, and the web UI.

`deploy/k8s/overlays/local/` runs those exact manifests on a single-node cluster. It adds Postgres (pgvector) and MockLLM, uses locally built images, and supplies dev secrets:

```bash
make runtime-up-k8s    # macOS: Colima + k3s + ingress-nginx (or use kind / Docker Desktop + ingress-nginx)
make k8s-deploy        # build images (shared with the cluster runtime) + kubectl apply -k deploy/k8s/overlays/local
make k8s-test          # 19 checks incl. self-healing and zero-downtime rollout
make runtime-down      # remove the VM, images, packages and config
```

CI (`.github/workflows/ci.yml`) runs gofmt, vet, race tests against pgvector and Redis service containers, the 1 ms guardrail latency budget, a benchmark smoke run, typecheck, Jest, the Next.js build, Playwright, Docker image builds, a full `docker compose up` with the end-to-end smoke test, and a **kind** cluster deployment running `scripts/k8s-smoke.sh`.

## Repository layout

```text
cmd/{gateway,mockllm,bench}     binaries: gateway server, mock upstream, benchmark runner
internal/auth                   JWT (HS256/RS256/JWKS) + API-key L1/Redis/Postgres store + /v1/auth/token
internal/ratelimit              Redis Lua dual token bucket + in-memory fallback
internal/guardrails             PII (regex + Luhn) redaction, injection scoring, streaming redactor
internal/cache                  embedders, sharded LRU, Redis exact tier, pgvector HNSW semantic tier
internal/router                 provider adapters, SSE parsing, circuit breaker, failover
internal/telemetry              Prometheus metrics, rolling collector, SSE stream, audit logger
internal/gateway                request pipeline + dependency wiring
internal/{config,storage,mockllm}
migrations/001_init_pgvector.sql pgvector schema, HNSW index, tenants, API keys, guardrail audit
web/                            Next.js 16 + React 19 + Tailwind 4 control plane, Jest + Playwright
deploy/k8s/{base,overlays/local} Kubernetes manifests (production base + local-cluster overlay)
deploy/prometheus/              Prometheus scrape config
benchmarks/                     k6 script + results.json (generated by cmd/bench)
scripts/bootstrap-venv.sh       builds the .venv toolchain (Go, Postgres+pgvector, Redis, k6, kubectl)
scripts/dev-services.sh         starts/stops the local Postgres and Redis from .venv
scripts/compose-smoke.sh        end-to-end checks against a running stack (compose or Kubernetes)
scripts/k8s-smoke.sh            Kubernetes checks: Ingress, HPA, PDB, self-healing, rolling restart, Redis outage
scripts/runtime.sh              macOS: install/start/remove Colima + Docker (+ k3s) on demand
```

## Limitations

- The benchmark runs on a single machine against a mock upstream. Absolute RPS is bounded by the mock's ~168 ms latency × 100 workers on misses, and by shared CPU on hits. Treat the numbers as relative, not production capacity.
- The hashing embedder measures lexical similarity. Deep paraphrases need the pluggable HTTP embedder.
- pgvector 0.6's HNSW applies `WHERE` filters after the index scan. For very large multi-tenant tables, use partial indexes per large tenant, or pgvector ≥ 0.8 iterative scans.
- Kubernetes is verified on single-node clusters (k3s and kind), not on a managed multi-node cloud cluster. Zone spreading and autoscaling *under real load* therefore aren't exercised; the HPA reading metrics and the PDB budget are.
- Dashboard telemetry (`/v1/telemetry/*`) is per replica: behind a load balancer, each dashboard connection shows the replica it reached. Prometheus, which scrapes every pod, is the source of fleet-wide totals.
