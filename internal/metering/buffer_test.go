package metering

import (
	"testing"
	"time"
)

// TestMetering_BufferFull_DropsWithoutBlocking covers AC-036: with a buffer of
// capacity N filled to capacity (no worker consuming), the (N+1)th Enqueue is
// dropped, metering_events_dropped_total increments by 1, and Enqueue returns
// immediately (non-blocking). We deliberately do NOT start a worker so the
// channel cannot drain — the buffer is genuinely full.
func TestMetering_BufferFull_DropsWithoutBlocking(t *testing.T) {
	t.Parallel()

	const n = 8
	rec := &countingDropRecorder{}
	buf := NewBuffer(n, rec)

	// Fill the buffer exactly to capacity. None of these should drop.
	for i := 0; i < n; i++ {
		buf.Enqueue(sampleEvent(i))
	}
	if rec.count() != 0 {
		t.Fatalf("dropped %d while filling to capacity, want 0", rec.count())
	}
	if buf.Len() != n {
		t.Fatalf("buffer len = %d, want %d (full)", buf.Len(), n)
	}

	// The (N+1)th enqueue must drop and must return well under a deadline (it is a
	// non-blocking select/default, so it returns essentially instantly).
	done := make(chan struct{})
	go func() {
		buf.Enqueue(sampleEvent(n))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked on a full buffer (hot path must never block, INV-003/CON-006)")
	}

	if got := rec.count(); got != 1 {
		t.Errorf("metering_events_dropped_total incremented by %d, want 1", got)
	}
	if buf.Len() != n {
		t.Errorf("buffer len = %d after drop, want %d (unchanged)", buf.Len(), n)
	}
}

// TestBuffer_NilRecorder asserts a nil recorder is tolerated (no-op) so the
// buffer is usable without metrics wiring.
func TestBuffer_NilRecorder(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(1, nil)
	buf.Enqueue(sampleEvent(0)) // fills
	buf.Enqueue(sampleEvent(1)) // drops, must not panic on nil recorder
}

// TestBuffer_NonPositiveCapacity asserts a non-positive capacity falls back to a
// usable buffer rather than a zero-capacity channel that would drop everything.
func TestBuffer_NonPositiveCapacity(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(0, nil)
	buf.Enqueue(sampleEvent(0))
	if buf.Len() != 1 {
		t.Errorf("buffer len = %d, want 1 (fallback capacity)", buf.Len())
	}
}
