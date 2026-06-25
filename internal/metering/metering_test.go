package metering

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// fakeRepo is a test double for MeteringRepository. It captures every flushed
// event (batched) and can be configured to fail or to block on flush so tests
// can exercise the worker's batch/drain/retry behaviour deterministically.
type fakeRepo struct {
	mu       sync.Mutex
	captured []UsageEvent
	batches  int

	// failN makes the first failN Flush calls return an error (AC-037). After
	// that, flushes succeed.
	failN int32
	// failAlways makes every Flush return an error.
	failAlways bool
	// block, when non-nil, is received-from at the start of every Flush so a test
	// can hold the worker inside a flush (to fill the buffer behind it).
	block chan struct{}
}

func (f *fakeRepo) Flush(ctx context.Context, events []UsageEvent) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.failAlways {
		return errors.New("fake: postgres down")
	}
	if atomic.LoadInt32(&f.failN) > 0 {
		atomic.AddInt32(&f.failN, -1)
		return errors.New("fake: postgres down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captured = append(f.captured, events...)
	f.batches++
	return nil
}

func (f *fakeRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.captured)
}

func (f *fakeRepo) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batches
}

// countingDropRecorder counts dropped events (the metering_events_dropped_total
// signal) without importing Prometheus.
type countingDropRecorder struct{ dropped int64 }

func (c *countingDropRecorder) IncMeteringEventsDropped() { atomic.AddInt64(&c.dropped, 1) }
func (c *countingDropRecorder) count() int64              { return atomic.LoadInt64(&c.dropped) }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
