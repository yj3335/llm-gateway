// Package storage manages the PostgreSQL connection pool and schema
// migrations shared by the auth key store, semantic cache, and audit log.
package storage

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/yj3335/llm-gateway/migrations"
)

// Options configures Connect.
type Options struct {
	DSN      string
	MaxConns int32
	// MinConns connections are opened and warmed at startup so bursts never
	// pay connection setup + pgvector type registration in the request path.
	MinConns     int32
	Migrate      bool
	HNSWEfSearch int
}

// Connect applies migrations (optionally) and returns a pool whose
// connections understand the pgvector `vector` type.
func Connect(ctx context.Context, o Options) (*pgxpool.Pool, error) {
	if o.Migrate {
		if err := Migrate(ctx, o.DSN); err != nil {
			return nil, err
		}
	}
	cfg, err := pgxpool.ParseConfig(o.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	if o.MinConns > 0 {
		cfg.MinConns = min(o.MinConns, cfg.MaxConns)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvec.RegisterTypes(ctx, conn); err != nil {
			return fmt.Errorf("register pgvector types: %w", err)
		}
		if o.HNSWEfSearch > 0 {
			if _, err := conn.Exec(ctx, "SET hnsw.ef_search = "+strconv.Itoa(o.HNSWEfSearch)); err != nil {
				return fmt.Errorf("set hnsw.ef_search: %w", err)
			}
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	warm(ctx, pool, int(cfg.MinConns))
	return pool, nil
}

// warm opens n connections concurrently, then returns them to the pool.
func warm(ctx context.Context, pool *pgxpool.Pool, n int) {
	conns := make(chan *pgxpool.Conn, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := pool.Acquire(ctx); err == nil {
				conns <- c
			}
		}()
	}
	wg.Wait()
	close(conns)
	for c := range conns {
		c.Release()
	}
}

// Migrate applies every embedded migration in lexical order. Migrations are
// written to be idempotent; an advisory lock serializes concurrent replicas.
func Migrate(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("migrate connect: %w", err)
	}
	defer conn.Close(ctx)
	const lockID = 7_311_402_001
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return fmt.Errorf("migrate lock: %w", err)
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockID) //nolint:errcheck

	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		sql, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}
