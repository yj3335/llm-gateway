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
-- 384-dim embeddings from the deterministic feature-hashing embedder (or an
-- external embedding API configured with dimensions=384).
CREATE TABLE IF NOT EXISTS semantic_cache (
    id                BIGSERIAL   PRIMARY KEY,
    tenant_id         TEXT        NOT NULL,
    model             TEXT        NOT NULL,
    -- Hash of system prompt + prior turns: a semantic match is only valid when
    -- the surrounding conversation context is identical.
    scope_hash        CHAR(64)    NOT NULL,
    prompt_hash       CHAR(64)    NOT NULL,
    prompt_text       TEXT        NOT NULL,
    embedding         vector(384) NOT NULL,
    response_payload  JSONB       NOT NULL,
    prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
    completion_tokens INTEGER     NOT NULL DEFAULT 0,
    hit_count         BIGINT      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours',
    CONSTRAINT semantic_cache_unique UNIQUE (tenant_id, model, prompt_hash)
);

-- HNSW approximate-nearest-neighbour index on cosine distance.
CREATE INDEX IF NOT EXISTS semantic_cache_embedding_hnsw
    ON semantic_cache USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
CREATE INDEX IF NOT EXISTS semantic_cache_scope_idx ON semantic_cache (tenant_id, model, scope_hash);
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
