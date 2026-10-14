# Benchmarks

Two benchmarks, both produced by `cmd/bench` and stored in `benchmarks/`. Back to the [README](../README.md).

| | Real models (headline) | Gateway capacity |
|---|---|---|
| Command | `make bench` | `make bench-mock` |
| Upstream | local Ollama: `qwen3.5:9b` → `gemma4:e4b` | in-process mock LLM (~170 ms) |
| Cache embedder | `nomic-embed-text` (768-d) | hashing (384-d) |
| Load | 1,000 requests, 4 concurrent clients | 5,000 requests, 100 concurrent clients |
| Results | [`results.json`](../benchmarks/results.json) | [`results-mock.json`](../benchmarks/results-mock.json) |

Both use the same workload: 300 question intents (60 topics × 5 question types) × 4 human-style paraphrases with **zipf (s = 1.1)** popularity, plus 25% never-repeating questions, 10% prompts containing PII, 2% prompt-injection attempts, and **10% of calls to the primary model failing** with an injected 503. Real Redis 7.4 and PostgreSQL 16 + pgvector run on the same machine. Cache precision is checked on every semantic hit: the returned answer must have been produced for the same question intent.

Environment: Apple Silicon laptop, 15 CPUs, macOS, Go 1.27.2. The benchmark client, gateway, databases and Ollama share the machine.

## Real models (local Ollama)

Ollama on this laptop generates one answer at a time (about 0.7 answers/s, ~40 tokens/s; more concurrency only adds queueing), so throughput is bounded by the model, not the gateway. Answers are capped at 64 tokens.

| Latency | p50 | p95 | p99 | Notes |
|---|---:|---:|---:|---|
| Model alone, one request at a time (no gateway) | 2,066.1 ms | 2,125.5 ms | 2,146.7 ms | pure generation latency |
| Cache hit, end to end | 16.4 ms | 28.4 ms | 35.0 ms |  |
| Cache miss, end to end | 7,806.6 ms | 8,504.8 ms | 9,175.2 ms | includes queueing behind other requests |
| Gateway overhead, all requests | 16.4 ms | 25.8 ms | 35.5 ms | mostly the nomic embedding for the semantic lookup |
| Hot cache (exact hits) | 0.14 ms | 0.17 ms | 0.23 ms | 12,166 req/s |

| Effectiveness | Value |
|---|---|
| Cache hit ratio | **53.92%** (exact 14.48%, semantic 39.45%); 71.9% on repeated questions |
| Semantic precision | **100%** of 380 verified semantic hits (average similarity 0.9647) |
| Latency, hit vs miss | **99.79%** lower at p50 (16.4 ms vs 7.81 s) |
| Mean latency, all requests | **53.82%** lower than an all-miss workload |
| Model calls avoided | 531 of 1000 requests answered without generation |
| Failover | **100%**: 39 injected faults on Qwen, all answered by Gemma, 0 errors to clients |
| Guardrails | 100/100 PII prompts redacted; 19/19 injections blocked; 0 false positives |
| Throughput | 1.2 req/s mixed (bounded by one local GPU); 12,166 req/s from cache |

**Reading the numbers.** A cache hit costs about 16 ms instead of about 7.8 s of queued generation. The gateway's own overhead (~16 ms at p50) is almost entirely the neural embedding computed for each non-exact lookup; exact hits cost 0.14 ms. Compared with the hashing embedder, `nomic-embed-text` recognizes far more paraphrases (semantic hits 39.45% vs 23.55%) with no wrong answers.

## Gateway capacity (mock upstream)

The mock LLM answers in ~170 ms and never queues, which isolates the gateway itself under 100 concurrent clients.

| Latency | p50 | p95 | p99 | Notes |
|---|---:|---:|---:|---|
| Mock upstream alone | 168.8 ms | 184.8 ms | 188.2 ms |  |
| Gateway overhead, all requests | 0.41 ms | 1.05 ms | 23.26 ms |  |
| Cache hit, end to end | 0.46 ms | 0.93 ms | 1.59 ms |  |
| Hot cache (exact hits) | 1.38 ms | 2.28 ms | 3.18 ms | 30,087 req/s |

| Effectiveness | Value |
|---|---|
| Throughput | **1,525.7 req/s** mixed; **30,087 req/s** from cache |
| Cache hit ratio | 61.76% (exact 38.22%, semantic 23.55%), precision 98.61% of 1151 |
| Failover | 100%: 188 injected faults, 0 errors to clients |
| Guardrails | 561/561 PII redacted; 112/112 injections blocked; 0 false positives |

The p99 overhead (~23 ms) is pgvector search competing for CPU with 100 in-flight requests and the load generator on one laptop.

## Micro-benchmarks

| Component | p50 | p99 |
|---|---:|---:|
| Guardrails, typical prompt | 3.0 µs | 10.6 µs |
| Guardrails, 3.6 KB prompt with PII | 91 µs | 115 µs |
| Hashing embedder (fallback) | 2.1 µs | 5.1 µs |

## Load tests (k6)

`benchmarks/k6-load-test.js` (`make k6`, against `make run-mock`), 10-second runs, all thresholds passing:

| Target | Throughput | Gateway overhead p95 | Client latency p95 |
|---|---:|---:|---:|
| Native gateway processes (15 CPUs) | 6,863 req/s | 0.75 ms | 1.38 ms |
| `docker compose` stack in a 2-CPU / 4 GB Linux VM | 3,310 req/s | 1.20 ms | 3.38 ms |

Under that load the compose stack used about 220 MB of RAM.
