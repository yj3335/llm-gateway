package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GuardrailEvent is persisted to the guardrail_audit table.
type GuardrailEvent struct {
	RequestID string
	TenantID  string
	Direction string // input | output
	Category  string
	Action    string // REDACT | BLOCK | FLAG
	Score     float64
	Detail    map[string]any
	At        time.Time
}

// Auditor writes structured JSON audit records and (optionally) batches
// guardrail events into PostgreSQL.
type Auditor struct {
	log  *slog.Logger
	pool *pgxpool.Pool

	ch     chan GuardrailEvent
	wg     sync.WaitGroup
	mu     sync.RWMutex
	closed bool
}

// NewAuditor writes JSON lines to w. pool may be nil.
func NewAuditor(w io.Writer, pool *pgxpool.Pool) *Auditor {
	a := &Auditor{log: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})), pool: pool}
	if pool != nil {
		a.ch = make(chan GuardrailEvent, 8192)
		a.wg.Add(1)
		go a.sink()
	}
	return a
}

// Request logs one completed request.
func (a *Auditor) Request(t Trace) {
	attrs := []any{
		"request_id", t.ID, "tenant_id", t.TenantID, "model", t.Model, "provider", t.Provider,
		"status", t.Status, "cache", t.CacheStatus, "similarity", t.Similarity,
		"latency_ms", t.LatencyMs, "overhead_ms", t.OverheadMs, "upstream_ms", t.UpstreamMs,
		"guardrail_action", t.GuardrailAction, "failover", t.Failover, "stream", t.Stream,
		"prompt_tokens", t.PromptTokens, "completion_tokens", t.CompletionTokens,
	}
	if len(t.GuardrailTypes) > 0 {
		attrs = append(attrs, "guardrail_types", t.GuardrailTypes)
	}
	if t.Error != "" {
		attrs = append(attrs, "error", t.Error)
	}
	a.log.Info("request", attrs...)
}

// Guardrail logs a guardrail intervention and enqueues it for persistence.
func (a *Auditor) Guardrail(e GuardrailEvent) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	a.log.Warn("guardrail", "request_id", e.RequestID, "tenant_id", e.TenantID, "direction", e.Direction,
		"category", e.Category, "action", e.Action, "score", e.Score)
	if a.ch == nil {
		return
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		return
	}
	select {
	case a.ch <- e:
	default: // never block the request path on audit persistence
	}
}

// Event logs an arbitrary operational event.
func (a *Auditor) Event(msg string, attrs ...any) { a.log.Info(msg, attrs...) }

func (a *Auditor) sink() {
	defer a.wg.Done()
	batch := make([]GuardrailEvent, 0, 256)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		rows := make([][]any, len(batch))
		for i, e := range batch {
			detail, _ := json.Marshal(e.Detail)
			if e.Detail == nil {
				detail = []byte("{}")
			}
			rows[i] = []any{e.At, e.RequestID, e.TenantID, e.Direction, e.Category, e.Action, e.Score, detail}
		}
		_, err := a.pool.CopyFrom(ctx, pgx.Identifier{"guardrail_audit"},
			[]string{"ts", "request_id", "tenant_id", "direction", "category", "action", "score", "detail"}, pgx.CopyFromRows(rows))
		cancel()
		if err != nil {
			a.log.Error("guardrail audit persist failed", "err", err, "events", len(batch))
		}
		batch = batch[:0]
	}
	for {
		select {
		case e, ok := <-a.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= 256 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// Close flushes pending events.
func (a *Auditor) Close() {
	if a.ch == nil {
		return
	}
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.ch)
	}
	a.mu.Unlock()
	a.wg.Wait()
}
