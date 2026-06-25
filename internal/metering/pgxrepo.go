package metering

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Execer is the narrow pgx surface the repository depends on (ADR-0010): the
// ability to send a pgx.Batch. *pgxpool.Pool satisfies it, as does *pgx.Conn and
// pgx.Tx, so the adapter never depends on a concrete pool type and tests can
// substitute a fake. Keeping this narrow keeps the metering package free of any
// concrete pgx client wiring.
type Execer interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// insertSQL is the single-row INSERT queued once per event in the batch. Using a
// pgx.Batch pipelines all rows in one network round-trip while keeping the
// statement a plain parameterised INSERT (no string building → no injection).
const insertSQL = `INSERT INTO usage_events
	(provider, model, prompt_tokens, completion_tokens, total_tokens, latency_ms, status, request_id, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// PgxRepository is the pgx/v5 implementation of MeteringRepository (COMP-018,
// ADR-0010). It batch-INSERTs usage events into the usage_events table via a
// pipelined pgx.Batch. The pgx Execer is INJECTED (no global, no concrete pool
// dependency beyond the narrow interface).
type PgxRepository struct {
	db Execer
}

// NewPgxRepository constructs the pgx-backed repository over the injected Execer
// (typically a *pgxpool.Pool built in cmd/gateway).
func NewPgxRepository(db Execer) *PgxRepository {
	return &PgxRepository{db: db}
}

// Flush persists every event in the batch in a single pipelined round-trip. It
// returns an error if any row fails so the worker can retry or drop-with-log
// (AC-037); on success all rows are committed. It honours ctx for cancellation
// (the worker bounds it with a timeout).
func (r *PgxRepository) Flush(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, e := range events {
		batch.Queue(insertSQL,
			e.Provider,
			e.Model,
			e.PromptTokens,
			e.CompletionTokens,
			e.TotalTokens,
			e.Latency.Milliseconds(),
			e.Status,
			e.RequestID,
			e.Timestamp,
		)
	}

	results := r.db.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()

	for i := range events {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("metering: insert usage event %d/%d: %w", i+1, len(events), err)
		}
	}
	return nil
}

var _ MeteringRepository = (*PgxRepository)(nil)
