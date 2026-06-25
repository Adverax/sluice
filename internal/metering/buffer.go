package metering

// Buffer is COMP-016, the Usage Buffer: a bounded channel between the request
// hot path (producers calling Enqueue) and the single Metering Worker
// (consumer). Its only job is to decouple the two so the hot path never blocks
// on Postgres (INV-003 / CON-006).
//
// Capacity comes from config (GATEWAY_METERING_BUFFER_SIZE, default 1000 per
// ADR-0005). Enqueue uses a non-blocking send (select/default): on a full
// channel the event is DROPPED and metering_events_dropped_total is incremented
// (ADR-0007, AC-036) — it is NEVER allowed to block the caller.
type Buffer struct {
	ch       chan UsageEvent
	recorder DropRecorder
}

// NewBuffer constructs a Usage Buffer with the given channel capacity. capacity
// MUST be > 0 (the caller validates it from config, fail-loud); a non-positive
// value falls back to 1 so the buffer is always usable. recorder may be nil, in
// which case drops are counted by a no-op (the buffer never imports Prometheus).
func NewBuffer(capacity int, recorder DropRecorder) *Buffer {
	if capacity <= 0 {
		capacity = 1
	}
	if recorder == nil {
		recorder = NopDropRecorder{}
	}
	return &Buffer{
		ch:       make(chan UsageEvent, capacity),
		recorder: recorder,
	}
}

// Enqueue implements Sink. It performs a NON-BLOCKING send: if the buffer has
// room the event is queued for the worker; otherwise it is dropped and the
// dropped counter is incremented. This is the one guarantee the whole design
// rests on — the request hot path returns immediately regardless of buffer or
// Postgres state (AC-036, INV-003 / CON-006).
func (b *Buffer) Enqueue(e UsageEvent) {
	select {
	case b.ch <- e:
		// queued for the worker.
	default:
		// buffer full: drop and count (ADR-0007 drop-on-full).
		b.recorder.IncMeteringEventsDropped()
	}
}

// Events returns the receive side of the channel for the worker to consume. It
// is unexported-by-intent at the API level (only the worker in this package
// uses it) but exposed as a method so the worker holds no struct field of the
// buffer's internals.
func (b *Buffer) Events() <-chan UsageEvent { return b.ch }

// Len reports the number of buffered-but-not-yet-consumed events. Used by tests
// and diagnostics; not part of the hot path.
func (b *Buffer) Len() int { return len(b.ch) }

var _ Sink = (*Buffer)(nil)
