// Package metering implements asynchronous usage persistence (FR-014), the
// CTX-004 metering context: COMP-016 Usage Buffer, COMP-017 Metering Worker and
// COMP-018 MeteringRepository. Usage of a completed inference is recorded on the
// request hot path via a NON-BLOCKING enqueue into a buffered channel, then a
// background worker batch-flushes events to Postgres. The hot path MUST NEVER
// block on metering (INV-003 / CON-006): when the buffer is full the event is
// DROPPED and a counter is incremented (ADR-0007 buffered_channel_drop_on_full,
// AC-036).
//
// Ports & adapters (forge:engineering-standards, ADR-0008/ADR-0010): this
// package imports NEITHER Prometheus NOR a concrete pgx client. The dropped
// counter is incremented through the narrow DropRecorder port (the metrics
// package's *Metrics satisfies it); the worker flushes through the
// MeteringRepository port, whose pgx/v5 adapter depends only on a narrow Execer
// interface (*pgxpool.Pool satisfies it). Tests substitute fakes for both.
package metering

import (
	"context"
	"time"
)

// UsageEvent is one unit of usage accounting captured on the request hot path
// after a completed inference. The worker batch-INSERTs these into the
// usage_events table (COMP-018). It carries only provider-agnostic, canonical
// values (ADR-0009) so the metering context never imports a provider package.
type UsageEvent struct {
	// Provider is the upstream provider/model alias that served the request
	// (the routing key from the model router, FR-002).
	Provider string
	// Model is the model that produced the completion (response.Model).
	Model string
	// PromptTokens / CompletionTokens / TotalTokens are the canonical token
	// accounting copied from provider.Usage.
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// Latency is the wall-clock time the gateway spent serving the request.
	Latency time.Duration
	// Status is the HTTP status code the gateway returned (e.g. 200).
	Status int
	// RequestID correlates the event with request logs/traces. May be empty.
	RequestID string
	// Timestamp is when the event was created (request completion time).
	Timestamp time.Time
}

// DropRecorder is the narrow metrics port the Usage Buffer depends on so it can
// increment metering_events_dropped_total WITHOUT importing Prometheus (ADR-0008
// boundary hygiene; mirrors the rejectRecorder pattern in internal/middleware).
// *metrics.Metrics satisfies it; NopDropRecorder lets callers stay
// un-instrumented in tests.
type DropRecorder interface {
	// IncMeteringEventsDropped records a single dropped usage event.
	IncMeteringEventsDropped()
}

// NopDropRecorder is a DropRecorder that discards every signal, so the enqueue
// path never needs a nil check.
type NopDropRecorder struct{}

// IncMeteringEventsDropped implements DropRecorder (no-op).
func (NopDropRecorder) IncMeteringEventsDropped() {}

var _ DropRecorder = NopDropRecorder{}

// Sink is the port the server depends on to record usage. It is intentionally
// one method so the server never sees the buffer/worker machinery and the hot
// path stays trivially non-blocking. *Buffer satisfies it; a NopSink lets the
// server run without metering wiring.
type Sink interface {
	// Enqueue records a usage event. It MUST return immediately and MUST NOT
	// block the caller (the request hot path): on a full buffer the event is
	// dropped (AC-036, INV-003 / CON-006).
	Enqueue(e UsageEvent)
}

// NopSink is a Sink that discards every event. It lets the server run
// un-metered (e.g. in unit tests) without a nil check on every call site.
type NopSink struct{}

// Enqueue implements Sink (no-op).
func (NopSink) Enqueue(UsageEvent) {}

var _ Sink = NopSink{}

// MeteringRepository is the persistence port (COMP-018, ADR-0010). The worker
// flushes a batch through it; the pgx/v5 adapter (PgxRepository) is the only
// production implementation. Flush MUST honour ctx so the worker's shutdown
// drain is bounded.
type MeteringRepository interface {
	// Flush persists a batch of usage events. It returns an error if the whole
	// batch could not be persisted; the worker treats a non-nil error as a
	// retryable failure (AC-037) and never silently drops without logging.
	Flush(ctx context.Context, events []UsageEvent) error
}
