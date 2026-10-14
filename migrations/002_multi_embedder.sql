-- 002_multi_embedder.sql
-- Multi-embedder semantic cache: lets several embedding models (different
-- dimensions) share semantic_cache. Upgrades databases created by the original
-- schema (embedding vector(384), one HNSW index) in place. Idempotent.

DO $$
BEGIN
    -- Original schema: drop the single-dimension HNSW index before widening the column.
    IF EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'semantic_cache_embedding_hnsw') THEN
        DROP INDEX semantic_cache_embedding_hnsw;
    END IF;

    -- vector(384) -> vector (no fixed dimension). atttypmod is -1 when dimensionless.
    IF EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'semantic_cache'::regclass AND attname = 'embedding' AND atttypmod <> -1
    ) THEN
        ALTER TABLE semantic_cache ALTER COLUMN embedding TYPE vector;
    END IF;

    -- The unique key now includes the embedder.
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'semantic_cache_unique') THEN
        ALTER TABLE semantic_cache DROP CONSTRAINT semantic_cache_unique;
    END IF;
END $$;

ALTER TABLE semantic_cache ADD COLUMN IF NOT EXISTS embedder TEXT NOT NULL DEFAULT 'hashing-ngram-384';
ALTER TABLE semantic_cache ADD COLUMN IF NOT EXISTS dims INTEGER NOT NULL DEFAULT 384;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'semantic_cache_embedder_unique') THEN
        ALTER TABLE semantic_cache
            ADD CONSTRAINT semantic_cache_embedder_unique UNIQUE (tenant_id, model, embedder, prompt_hash);
    END IF;
END $$;

DROP INDEX IF EXISTS semantic_cache_scope_idx;
CREATE INDEX IF NOT EXISTS semantic_cache_scope_embedder_idx
    ON semantic_cache (tenant_id, model, embedder, scope_hash);

-- Partial HNSW indexes for the two built-in configurations. Other dimensions
-- are created on demand by the gateway (PGVectorStore.EnsureIndex).
CREATE INDEX IF NOT EXISTS semantic_cache_hnsw_d384 ON semantic_cache
    USING hnsw ((embedding::vector(384)) vector_cosine_ops) WITH (m = 16, ef_construction = 64)
    WHERE dims = 384;
CREATE INDEX IF NOT EXISTS semantic_cache_hnsw_d768 ON semantic_cache
    USING hnsw ((embedding::vector(768)) vector_cosine_ops) WITH (m = 16, ef_construction = 64)
    WHERE dims = 768;
