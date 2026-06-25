package metering

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeBatchResults is a minimal pgx.BatchResults that returns a fixed error (or
// nil) for each Exec, so the repository's per-row error handling can be unit
// tested WITHOUT a live Postgres. The full live pgx INSERT against the real
// usage_events table is integration-tested in CARD-011 via testcontainers.
type fakeBatchResults struct {
	execErr error
	execN   int
}

func (f *fakeBatchResults) Exec() (pgconn.CommandTag, error) {
	f.execN++
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeBatchResults) Query() (pgx.Rows, error) { return nil, errors.New("unused") }
func (f *fakeBatchResults) QueryRow() pgx.Row        { return nil }
func (f *fakeBatchResults) Close() error             { return nil }

// fakeExecer captures the queued batch and returns a configured BatchResults.
type fakeExecer struct {
	lastBatch *pgx.Batch
	results   *fakeBatchResults
}

func (f *fakeExecer) SendBatch(_ context.Context, b *pgx.Batch) pgx.BatchResults {
	f.lastBatch = b
	return f.results
}

// TestPgxRepository_Flush_QueuesAllRows asserts the repository queues one INSERT
// per event and reports success when every Exec succeeds.
func TestPgxRepository_Flush_QueuesAllRows(t *testing.T) {
	t.Parallel()

	ex := &fakeExecer{results: &fakeBatchResults{}}
	repo := NewPgxRepository(ex)

	events := []UsageEvent{
		{Provider: "mock", Model: "mock", PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, Latency: 5 * time.Millisecond, Status: 200, RequestID: "r1", Timestamp: time.Unix(1, 0)},
		{Provider: "mock", Model: "mock", PromptTokens: 4, CompletionTokens: 5, TotalTokens: 9, Latency: 7 * time.Millisecond, Status: 200, RequestID: "r2", Timestamp: time.Unix(2, 0)},
	}
	if err := repo.Flush(context.Background(), events); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}
	if ex.lastBatch == nil || ex.lastBatch.Len() != len(events) {
		t.Fatalf("queued %v rows, want %d", ex.lastBatch, len(events))
	}
	if ex.results.execN != len(events) {
		t.Errorf("Exec called %d times, want %d (one per row)", ex.results.execN, len(events))
	}
}

// TestPgxRepository_Flush_PropagatesError asserts a per-row failure surfaces as
// an error so the worker can retry/drop-with-log (AC-037).
func TestPgxRepository_Flush_PropagatesError(t *testing.T) {
	t.Parallel()

	ex := &fakeExecer{results: &fakeBatchResults{execErr: errors.New("conn refused")}}
	repo := NewPgxRepository(ex)

	err := repo.Flush(context.Background(), []UsageEvent{sampleEvent(1)})
	if err == nil {
		t.Fatal("Flush returned nil, want error on a failing Exec")
	}
}

// TestPgxRepository_Flush_Empty asserts an empty batch is a no-op (no SendBatch).
func TestPgxRepository_Flush_Empty(t *testing.T) {
	t.Parallel()

	ex := &fakeExecer{results: &fakeBatchResults{}}
	repo := NewPgxRepository(ex)
	if err := repo.Flush(context.Background(), nil); err != nil {
		t.Fatalf("Flush(nil) returned error: %v", err)
	}
	if ex.lastBatch != nil {
		t.Error("SendBatch called for an empty batch, want no-op")
	}
}
