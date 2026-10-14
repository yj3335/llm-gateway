# Changelog

All notable changes to this project. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow [Semantic Versioning](https://semver.org/).
"Fixed" entries name the symptom, the root cause, and the fix. Every one was found by a measurement, a test, or a real deployment.

## [0.5.0] - 2026-10-14

### Changed
- The headline benchmark runs on the real models (local Ollama: `qwen3.5:9b` → `gemma4:e4b`, `nomic-embed-text` embeddings) instead of a mock LLM. The mock run is kept as a gateway-capacity measurement (`benchmarks/results-mock.json`).
- Cache precision in the benchmark is verified by mapping each cached answer back to the question that first produced it, instead of relying on the mock echoing the prompt. Works for real models.
- Documentation split: the README is a front page; details live in `docs/` and this changelog.

### Added
- `fault_injection_rate` per provider (chaos testing): a fraction of calls fail with a synthetic 503, for benchmarks and drills against real upstreams.
- Cache options for a request-path embedding budget (`embedding_timeout`) and a separate background-write timeout.

### Fixed
- Under load on real models, gateway overhead p95 reached 2 s and semantic hits collapsed. Cause: on one Ollama, embedding calls queue behind generation; lookups timed out, and each timeout switched to the hashing fallback, splitting the cache across two embedding spaces. Fix: a busy embedder skips the semantic lookup for that request only; only connection failures trigger the fallback; background writes embed without the request deadline. Same pilot: overhead p95 2,001 ms → 31.5 ms, semantic hits 5% → 17%.

## [0.4.0] - 2026-10-14

### Added
- Default configuration runs on local Ollama with zero paid API keys: `qwen3.5:9b` primary, `gemma4:e4b` fallback, `nomic-embed-text` semantic-cache embeddings (`configs/gateway.yaml`). The mock-upstream config moved to `configs/gateway.mock.yaml`.
- Ollama embedder with automatic fallback to the deterministic hashing embedder while Ollama is unreachable; embedder status in `/readyz`; `llmgateway_embedder_fallbacks_total`.
- OpenAI-compatible `POST /v1/embeddings` served by the configured embedder.
- Per-provider `extra_body`; `X-Gateway-Chaos: timeout-primary`; `LLMGW_OLLAMA_URL`; `.env.example`.
- `make ollama-pull`, `make ollama-check` (live test through local Ollama), `make run-mock`.
- Migration `002_multi_embedder.sql`: embeddings of different sizes share one table, with a partial HNSW index per dimension; upgrades existing databases in place.

### Changed
- Semantic cache entries are scoped by embedder, with a threshold per embedder: 0.92 for `nomic-embed-text` (calibrated: paraphrases ≥ 0.948, near-misses ≤ 0.890), 0.86 for hashing.
- Chaos faults are synthesized inside the gateway instead of forwarding a header that only the mock understood.

### Fixed
- Qwen 3.5 and Gemma 4 returned empty answers. Cause: "thinking" mode spent the whole `max_tokens` budget; Ollama ignores `think: false` on its OpenAI-compatible endpoint. Fix: `extra_body: {reasoning_effort: none}`, enforced by a config test.
- Ollama `/api/embeddings` vectors had norm ≈ 22.8. Cause: the endpoint does not normalize. Fix: L2-normalize (with optional Matryoshka truncation).
- The hashing threshold (0.86) would have served "TCP flow control" the cached answer for "TCP congestion control" (nomic similarity 0.890). Fix: per-embedder thresholds (above).
- `X-Gateway-Chaos: fail-primary` did not fail real providers. Fix: gateway-synthesized faults (above).
- Non-streaming responses lacked `X-Gateway-Upstream-Model` and `X-Gateway-Attempts`. Cause: only the streaming path set them. Found by the live Ollama test.

## [0.3.1] - 2026-10-13

### Fixed
- Intermittent CI failure in the Kubernetes test ("Ingress routes to the gateway"). Cause: ingress-nginx answers 404/503 for a few seconds after an Ingress is applied, and the readiness loop accepted any HTTP status. Fix: wait for a real 200.

## [0.3.0] - 2026-10-13

### Added
- Kubernetes verified on a live cluster: `deploy/k8s/base` (production) plus `deploy/k8s/overlays/local`, and `scripts/k8s-smoke.sh` (Ingress load balancing, HPA, PDB, pod self-healing, zero-downtime rolling restart, Redis outage). Runs on kind in CI.
- `server.dependency_wait`: the gateway retries Redis and PostgreSQL at startup.
- `scripts/runtime.sh` and `make runtime-*`: install, start, and remove Colima + Docker (+ k3s, ingress-nginx) on demand.

### Fixed
- Every gateway and web pod failed with `CreateContainerConfigError`. Cause: images declared `USER nonroot` / `USER node`, which the kubelet cannot verify under `runAsNonRoot: true` (Docker Compose does not check). Fix: numeric UIDs in the images and pod specs.
- Possible silent degradation when the gateway started before PostgreSQL. Cause: containers start in any order. Fix: `dependency_wait`.
- Test checks failed although they matched. Cause: under `set -o pipefail`, `grep -q` exits early and `kubectl logs` gets SIGPIPE; `curl --max-time` on an endless SSE stream always exits 28. Fix: read full input; capture the stream to a file first.
- `kubectl apply -k` reported a cycle. Cause: the overlay lived inside its base. Fix: `base/` + `overlays/` layout.
- `make runtime-up-k8s` failed with "kubectl not found". Cause: Colima checks for `kubectl` before starting k3s, and it lives in `.venv`. Fix: activate `.venv` first.

### Changed
- Kubernetes metrics checks aggregate across pods: each replica counts only its own traffic (dashboard data is per replica; Prometheus gives fleet totals).

## [0.2.0] - 2026-10-13

### Added
- `scripts/compose-smoke.sh`: end-to-end checks against the Docker Compose stack, run in CI.

### Changed
- Docker builder pinned to `golang:1.27.2-alpine` to match `go.mod` (dependencies require Go ≥ 1.26).

### Fixed
- Tests at risk on 2-vCPU CI runners: exact token-bucket admission (token refill during a slow test) and the 1 ms guardrail budget under `-race`. Fix: refill-aware bounds; the latency budget is enforced in a separate non-race step.

## [0.1.0] - 2026-10-13

### Added
- OpenAI-compatible gateway in Go: JWT / OAuth2 (JWKS) / API-key auth, Redis Lua RPM + TPM rate limiting with a local fallback, PII redaction and prompt-injection guardrails, exact + pgvector semantic cache, request coalescing, multi-provider router with circuit breakers and failover, Prometheus metrics, audit log, and SSE telemetry.
- Next.js control-plane dashboard with an interactive playground; Jest and Playwright tests.
- Benchmark runner, mock upstream, k6 script, Kubernetes manifests, Docker Compose, and CI.

### Fixed (found before the first release)
- The circuit breaker opened at a true 10% error rate. Cause: instant 503s completed ahead of slow successes, so a "consecutive failures" rule saw a burst. Fix: outcomes counted in admission order; trip on ≥ 50% failures over the last 20 calls (minimum 10).
- Cache-miss lookups had a 47 ms p95 tail. Cause: the connection pool opened connections lazily during bursts. Fix: pre-warmed `min_conns` (p95 → 4.6 ms) and batched hit counters.
- Guardrails took 1.2 ms on a 3.6 KB prompt. Cause: regexes scanned the whole prompt. Fix: keyword gates and windowed PII scanning (2.9 µs typical, 121 µs p99 at 3.6 KB).
- Playwright crashed on click on recent macOS. Cause: the stripped headless-shell Chromium. Fix: full Chromium in new-headless mode.

[0.5.0]: https://github.com/yj3335/llm-gateway/compare/53044f7...HEAD
[0.4.0]: https://github.com/yj3335/llm-gateway/compare/6259d88...53044f7
[0.3.1]: https://github.com/yj3335/llm-gateway/compare/0c26189...ca22969
[0.3.0]: https://github.com/yj3335/llm-gateway/compare/bbe9342...c45a1dc
[0.2.0]: https://github.com/yj3335/llm-gateway/compare/2ea36b5...bbe9342
[0.1.0]: https://github.com/yj3335/llm-gateway/commit/2ea36b5
