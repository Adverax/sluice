package provider

import (
	"context"
	"errors"
	"time"
)

// ErrMockFailure is the canonical error the Mock returns when it is configured
// to fail a call (see Mock.ErrorRate / WithError). Callers can match it with
// errors.Is.
var ErrMockFailure = errors.New("provider: mock configured failure")

// Mock is the v1 Provider implementation (ADR-0009): a configurable test double
// that stands in for a real LLM provider in proxy tests (CARD-003/004) without
// needing an HTTP server. It has no global state and is safe to construct per
// test.
//
// Behaviour is deterministic by construction: there is no randomness. ErrorRate
// is a deterministic gate (>= 1 always fails, <= 0 never fails) rather than a
// probability, so tests get reproducible results under -race. Latency is the
// simulated per-call upstream delay and is always honoured against ctx.
//
// Construct a Mock with New (applying functional options) or build the struct
// literal directly — the zero value is a usable, instantaneous, always-success
// provider that streams Response.Content split into StreamChunks deltas.
type Mock struct {
	// Response is the canonical Response returned by Infer and used as the
	// content source for InferStream.
	Response Response

	// Latency is the simulated upstream delay applied before Infer returns and
	// before each InferStream chunk is emitted. Honoured against ctx.
	Latency time.Duration

	// ErrorRate gates failure deterministically: a value >= 1 makes every call
	// fail with Err; a value <= 0 never fails. Values in between are treated as
	// "fail" to keep behaviour deterministic and conservative for tests.
	ErrorRate float64

	// Err is the error returned when ErrorRate triggers a failure. When nil,
	// ErrMockFailure is used.
	Err error

	// StreamChunks is the number of content deltas InferStream emits before the
	// terminal chunk. When <= 0 it defaults to 1. The Response.Content is split
	// as evenly as possible across these chunks.
	StreamChunks int
}

// Option configures a Mock via the functional-options pattern (CON-001).
type Option func(*Mock)

// WithResponse sets the canonical Response the Mock returns.
func WithResponse(resp Response) Option {
	return func(m *Mock) { m.Response = resp }
}

// WithLatency sets the simulated per-call upstream latency.
func WithLatency(d time.Duration) Option {
	return func(m *Mock) { m.Latency = d }
}

// WithError makes every call fail with err (ErrorRate forced to 1). If err is
// nil, ErrMockFailure is used.
func WithError(err error) Option {
	return func(m *Mock) {
		m.ErrorRate = 1
		m.Err = err
	}
}

// WithStreamChunks sets how many content deltas InferStream emits.
func WithStreamChunks(n int) Option {
	return func(m *Mock) { m.StreamChunks = n }
}

// New constructs a Mock, applying the given options over the zero value.
func New(opts ...Option) *Mock {
	m := &Mock{}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// failErr reports whether this call should fail and, if so, with which error.
func (m *Mock) failErr() (error, bool) {
	if m.ErrorRate <= 0 {
		return nil, false
	}
	if m.Err != nil {
		return m.Err, true
	}
	return ErrMockFailure, true
}

// Infer implements Provider on the unary path. It simulates Latency while
// honouring ctx (returning ctx.Err() if cancelled mid-latency), then returns
// either the configured Response or the configured error.
func (m *Mock) Infer(ctx context.Context, _ Request) (Response, error) {
	if err := sleepCtx(ctx, m.Latency); err != nil {
		return Response{}, err
	}
	if err, fail := m.failErr(); fail {
		return Response{}, err
	}
	return m.Response, nil
}

// InferStream implements Provider on the streaming path. A configured failure
// is reported synchronously as an initialisation error (no channel is
// returned). Otherwise it returns a channel on which a background goroutine
// emits StreamChunks content deltas — each preceded by a Latency delay — then a
// terminal chunk carrying Done and Usage, and finally CLOSES the channel.
//
// The goroutine selects on ctx.Done() at every step, so a cancelled ctx stops
// emission promptly and still closes the channel — no goroutine leak.
func (m *Mock) InferStream(ctx context.Context, _ Request) (<-chan Chunk, error) {
	if err, fail := m.failErr(); fail {
		return nil, err
	}

	n := m.StreamChunks
	if n <= 0 {
		n = 1
	}
	deltas := splitContent(m.Response.Content, n)

	out := make(chan Chunk)
	go func() {
		defer close(out)
		for _, delta := range deltas {
			if err := sleepCtx(ctx, m.Latency); err != nil {
				return // ctx cancelled: stop emitting, channel closed by defer.
			}
			select {
			case out <- Chunk{Content: delta}:
			case <-ctx.Done():
				return
			}
		}
		// Terminal chunk: carries the normalised usage for metering (ADR-0009).
		select {
		case out <- Chunk{Done: true, Usage: m.Response.Usage}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// sleepCtx waits for d while honouring ctx. It returns ctx.Err() if ctx is
// cancelled before d elapses, and nil otherwise. A non-positive d is a no-op
// but still observes an already-cancelled ctx.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// Surface an already-cancelled context even with zero latency.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// splitContent divides s into n non-empty deltas as evenly as possible by rune
// count. When s is empty it returns n empty deltas so the stream still emits n
// chunks. n is assumed >= 1.
func splitContent(s string, n int) []string {
	if n <= 1 {
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return make([]string, n)
	}
	out := make([]string, 0, n)
	per := len(runes) / n
	rem := len(runes) % n
	idx := 0
	for i := 0; i < n; i++ {
		size := per
		if i < rem {
			size++
		}
		out = append(out, string(runes[idx:idx+size]))
		idx += size
	}
	return out
}
