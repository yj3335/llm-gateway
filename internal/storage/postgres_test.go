package storage

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// tempDB creates a throwaway database and returns its DSN (dropped on cleanup).
// Runs only when LLMGW_TEST_POSTGRES_DSN is set (make test / CI).
func tempDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LLMGW_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("LLMGW_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("llmgw_migtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close(context.Background())
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func q(t *testing.T, conn *pgx.Conn, sql string) string {
	t.Helper()
	var v any
	if err := conn.QueryRow(context.Background(), sql).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return fmt.Sprint(v)
}

func assertMultiEmbedderSchema(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if got := q(t, conn, `SELECT atttypmod FROM pg_attribute WHERE attrelid='semantic_cache'::regclass AND attname='embedding'`); got != "-1" {
		t.Fatalf("embedding column must be dimensionless, atttypmod=%s", got)
	}
	for _, idx := range []string{"semantic_cache_hnsw_d384", "semantic_cache_hnsw_d768", "semantic_cache_scope_embedder_idx"} {
		if q(t, conn, "SELECT count(*) FROM pg_indexes WHERE indexname='"+idx+"'") != "1" {
			t.Fatalf("missing index %s", idx)
		}
	}
	if q(t, conn, "SELECT count(*) FROM pg_indexes WHERE indexname='semantic_cache_embedding_hnsw'") != "0" {
		t.Fatal("old single-dimension HNSW index still present")
	}
	if q(t, conn, "SELECT count(*) FROM pg_constraint WHERE conname='semantic_cache_embedder_unique'") != "1" ||
		q(t, conn, "SELECT count(*) FROM pg_constraint WHERE conname='semantic_cache_unique'") != "0" {
		t.Fatal("unique constraint not migrated to include the embedder")
	}
}

func TestMigrateFreshDatabase(t *testing.T) {
	dsn := tempDB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // idempotent: every gateway start re-runs migrations
		if err := Migrate(ctx, dsn); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	assertMultiEmbedderSchema(t, conn)

	// Mixed dimensions coexist; each partial index serves its own dimension.
	if _, err := conn.Exec(ctx, `
INSERT INTO semantic_cache (tenant_id, model, embedder, dims, scope_hash, prompt_hash, prompt_text, embedding, response_payload)
VALUES ('t','m','hashing-ngram-384',384,'s',repeat('a',64),'p',('['||repeat('0.1,',383)||'0.1]')::vector,'{}'),
       ('t','m','ollama:nomic-embed-text',768,'s',repeat('a',64),'p',('['||repeat('0.1,',767)||'0.1]')::vector,'{}')`); err != nil {
		t.Fatalf("insert mixed dimensions: %v", err)
	}
}

func TestMigrateUpgradesOriginalSchema(t *testing.T) {
	dsn := tempDB(t)
	ctx := context.Background()
	v1, err := os.ReadFile("testdata/v1_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, string(v1)); err != nil {
		t.Fatalf("apply original schema: %v", err)
	}
	if !strings.Contains(q(t, conn, "SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid='semantic_cache'::regclass AND attname='embedding'"), "384") {
		t.Fatal("fixture is not the original vector(384) schema")
	}
	if _, err := conn.Exec(ctx, `
INSERT INTO semantic_cache (tenant_id, model, scope_hash, prompt_hash, prompt_text, embedding, response_payload)
VALUES ('t','m','s',repeat('b',64),'old row',('['||repeat('0.1,',383)||'0.1]')::vector,'{"content":"x"}')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, dsn); err != nil {
			t.Fatalf("migrate run %d over original schema: %v", i+1, err)
		}
	}
	assertMultiEmbedderSchema(t, conn)
	if got := q(t, conn, "SELECT embedder||'/'||dims FROM semantic_cache WHERE prompt_text='old row'"); got != "hashing-ngram-384/384" {
		t.Fatalf("existing row not backfilled: %s", got)
	}
}
