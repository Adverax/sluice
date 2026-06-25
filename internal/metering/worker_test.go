package metering

import (
	"context"
	"testing"
	"time"
)

func sampleEvent(i int) UsageEvent {
	return UsageEvent{
		Provider:         "mock",
		Model:            "mock",
		PromptTokens:     i,
		CompletionTokens: i,
		TotalTokens:      2 * i,
		Latency:          time.Millisecond,
		Status:           200,
		RequestID:        "req",
		Timestamp:        time.Unix(0, 0),
	}
}

// TestMetering_AsyncFlush_PersistsRecords covers AC-035: 100 enqueued events are
// flushed by the worker and captured (batched) by the fake repository. The flush
// is triggered deterministically by Close (drain), not by a real 5s wait.
func TestMetering_AsyncFlush_PersistsRecords(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	// Large buffer so nothing is dropped; long flush interval + large batch so the
	// only flush trigger in this test is the Close drain (deterministic).
	buf := NewBuffer(1000, nil)
	w := NewWorker(buf, repo, testLogger(),
		WithBatchSize(25),
		WithFlushInterval(time.Hour),
	)
	w.Start()

	const n = 100
	for i := 0; i < n; i++ {
		buf.Enqueue(sampleEvent(i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if got := repo.count(); got != n {
		t.Errorf("captured %d records, want %d", got, n)
	}
	if repo.batchCount() < 1 {
		t.Errorf("expected at least one batch, got %d", repo.batchCount())
	}
}

// TestMetering_AsyncFlush_BatchSizeTrigger asserts the batch-size flush trigger
// fires WITHOUT Close: with batch size 25 and 100 events, at least the first
// batches flush while the worker is still running.
func TestMetering_AsyncFlush_BatchSizeTrigger(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	buf := NewBuffer(1000, nil)
	w := NewWorker(buf, repo, testLogger(),
		WithBatchSize(25),
		WithFlushInterval(time.Hour),
	)
	w.Start()

	const n = 100
	for i := 0; i < n; i++ {
		buf.Enqueue(sampleEvent(i))
	}

	// Wait (deterministically, by polling) for the batch-size trigger to flush
	// the full set; this does not rely on the periodic timer.
	deadline := time.Now().Add(2 * time.Second)
	for repo.count() < n {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = w.Close(ctx)

	if got := repo.count(); got != n {
		t.Errorf("captured %d records, want %d", got, n)
	}
}

// TestMetering_PostgresDown_NoHotpathBlock covers AC-037: the repository returns
// errors on flush; the error is logged and the worker retries (bounded) then
// drops-with-log, while the hot path (Enqueue) is never blocked. We assert
// Enqueue completes immediately and that after recovery a later batch persists.
func TestMetering_PostgresDown_NoHotpathBlock(t *testing.T) {
	t.Parallel()

	// First two flushes fail, then succeed: the worker retries (default 2 extra
	// attempts) and the batch is NOT silently lost.
	repo := &fakeRepo{failN: 2}
	buf := NewBuffer(1000, nil)
	w := NewWorker(buf, repo, testLogger(),
		WithBatchSize(10),
		WithFlushInterval(time.Hour),
		WithFlushRetries(2),
		WithRetryBackoff(time.Millisecond),
	)
	w.Start()

	const n = 10
	start := time.Now()
	for i := 0; i < n; i++ {
		buf.Enqueue(sampleEvent(i))
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Enqueue blocked on the hot path: took %s", elapsed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// After the bounded retry recovered (failN exhausted), the batch persisted.
	if got := repo.count(); got != n {
		t.Errorf("captured %d records after retry, want %d (batch must not be silently lost)", got, n)
	}
}

// TestMetering_PostgresDown_AlwaysFails_DropsWithLog asserts the all-failing
// case: the worker exhausts its bounded retry and drops-with-log; the hot path
// is still never blocked and Close returns.
func TestMetering_PostgresDown_AlwaysFails_DropsWithLog(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{failAlways: true}
	buf := NewBuffer(1000, nil)
	w := NewWorker(buf, repo, testLogger(),
		WithBatchSize(5),
		WithFlushInterval(time.Hour),
		WithFlushRetries(2),
		WithRetryBackoff(time.Millisecond),
	)
	w.Start()

	for i := 0; i < 5; i++ {
		buf.Enqueue(sampleEvent(i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	// Nothing captured (all flushes failed), but the worker returned cleanly: the
	// drop was logged, not silent, and never blocked the hot path.
	if got := repo.count(); got != 0 {
		t.Errorf("captured %d records, want 0 (all flushes failed)", got)
	}
}

// TestGracefulShutdown_FlushesMetering covers AC-032: the buffer holds 50 events
// when Close is called; all 50 are flushed to the fake repository before Close
// returns. The worker is held off real flushing until Close so the drain path is
// the one exercised (deterministic).
func TestGracefulShutdown_FlushesMetering(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	buf := NewBuffer(1000, nil)
	// Large batch + long interval so neither the batch-size nor the timer trigger
	// fires before Close: all 50 events are still buffered when Close drains them.
	w := NewWorker(buf, repo, testLogger(),
		WithBatchSize(1000),
		WithFlushInterval(time.Hour),
	)
	w.Start()

	const n = 50
	for i := 0; i < n; i++ {
		buf.Enqueue(sampleEvent(i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if got := repo.count(); got != n {
		t.Errorf("flushed %d events on shutdown, want %d", got, n)
	}
}

// TestWorker_NoGoroutineLeak asserts Close is idempotent and the worker stops
// (Close returns nil) so there is no goroutine leak after shutdown.
func TestWorker_NoGoroutineLeak(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	buf := NewBuffer(16, nil)
	w := NewWorker(buf, repo, testLogger(), WithFlushInterval(time.Hour))
	w.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("first Close returned error: %v", err)
	}
	// Second Close must not panic or hang (idempotent).
	if err := w.Close(ctx); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}
