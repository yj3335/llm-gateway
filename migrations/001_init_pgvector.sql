-- 001_init_pgvector.sql
-- LLM Gateway schema: pgvector semantic cache (HNSW / cosine), tenant quotas,
-- hashed API keys, and the Responsible-AI guardrail audit trail.
-- Idempotent: safe to run on every gateway start.

CREATE EXTENSION IF NOT EXISTS vector;

-- Tenants & quota policy -------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenants (
    tenant_id   TEXT PRIMARY KEY,
    tier        TEXT        NOT NULL DEFAULT 'standard',
    rpm_limit   BIGINT      NOT NULL DEFAULT 600   CHECK (rpm_limit > 0),
    tpm_limit   BIGINT      NOT NULL DEFAULT 200000 CHECK (tpm_limit > 0),
    pii_mode    TEXT        NOT NULL DEFAULT 'REDACT' CHECK (pii_mode IN ('REDACT', 'BLOCK')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- API keys are stored only as SHA-256 hashes.
CREATE TABLE IF NOT EXISTS api_keys (
    key_hash    CHAR(64)    PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    key_prefix  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS api_keys_tenant_idx ON api_keys (tenant_id);

-- Semantic cache (L2) -----------------------------------------------------------
-- The embedding column is dimensionless so several embedders can share the
-- table (e.g. 768-d nomic-embed-text via Ollama and the 384-d hashing
-- fallback). Vectors are only compared within one embedder; each dimension
-- gets a partial HNSW expression index (see 002_multi_embedder.sql and
-- PGVectorStore.EnsureIndex).
CREATE TABLE IF NOT EXISTS semantic_cache (
    id                BIGSERIAL   PRIMARY KEY,
    tenant_id         TEXT        NOT NULL,
    model             TEXT        NOT NULL,
    embedder          TEXT        NOT NULL DEFAULT 'hashing-ngram-384',
    dims              INTEGER     NOT NULL DEFAULT 384,
    -- Hash of system prompt + prior turns: a semantic match is only valid when
    -- the surrounding conversation context is identical.
    scope_hash        CHAR(64)    NOT NULL,
    prompt_hash       CHAR(64)    NOT NULL,
    prompt_text       TEXT        NOT NULL,
    embedding         vector      NOT NULL,
    response_payload  JSONB       NOT NULL,
    prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
    completion_tokens INTEGER     NOT NULL DEFAULT 0,
    hit_count         BIGINT      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours',
    CONSTRAINT semantic_cache_embedder_unique UNIQUE (tenant_id, model, embedder, prompt_hash)
);
-- Indexes on the embedder/dims columns are created in 002, which also
-- upgrades databases created by the original single-dimension schema.
CREATE INDEX IF NOT EXISTS semantic_cache_expires_idx ON semantic_cache (expires_at);

-- Guardrail audit trail -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS guardrail_audit (
    id          BIGSERIAL   PRIMARY KEY,
    ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
    request_id  TEXT        NOT NULL,
    tenant_id   TEXT        NOT NULL,
    direction   TEXT        NOT NULL CHECK (direction IN ('input', 'output')),
    category    TEXT        NOT NULL,
    action      TEXT        NOT NULL CHECK (action IN ('REDACT', 'BLOCK', 'FLAG')),
    score       DOUBLE PRECISION NOT NULL DEFAULT 0,
    detail      JSONB       NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS guardrail_audit_tenant_ts_idx ON guardrail_audit (tenant_id, ts DESC);
CREATE INDEX IF NOT EXISTS guardrail_audit_category_idx ON guardrail_audit (category, ts DESC);
