package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/adverax/sluice/internal/breaker"
	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/metrics"
	"github.com/adverax/sluice/internal/pool"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/proxy/resilience"
	"github.com/adverax/sluice/internal/proxy/retry"
	"github.com/adverax/sluice/internal/server"
	"github.com/adverax/sluice/internal/tracing"

	"github.com/prometheus/client_golang/prometheus"
)

// --- CARD-014: streaming resilience seam ----------------------------------

// streamInitSpy is a streaming Provider double that records whether InferStream
// was invoked, so a fast-fail test can prove initiation never reached the
// provider (AC-014a/b). It can be configured to fail or to emit a small stream.
type streamInitSpy struct {
	initCalls atomic.Int32
	failInit  error            // when non-nil, InferStream returns this init error
	chunks    []provider.Chunk // content chunks to emit before the terminal Done
}

func (s *streamInitSpy) Infer(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, errors.New("unary not used in streaming resilience test")
}

func (s *streamInitSpy) InferStream(ctx context.Context, _ provider.Request) (<-chan provider.Chunk, error) {
	s.initCalls.Add(1)
	if s.failInit != nil {
		return nil, s.failInit
	}
	out := make(chan provider.Chunk)
	go func() {
		defer close(out)
		for _, c := range s.chunks {
			select {
			case out <- c:
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- provider.Chunk{Done: true, Usage: provider.Usage{TotalTokens: 7}}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// tripImmediately builds breaker settings that open after a single failure, so
// tests can force the open state deterministically without volume gating.
func tripImmediately(name string) gobreaker.Settings {
	return gobreaker.Settings{
		Name:        name,
		MaxRequests: 1,
		Timeout:     time.Hour, // stay open for the whole test (no half-open flapping).
		ReadyToTrip: func(c gobreaker.Counts) bool { return c.TotalFailures >= 1 },
	}
}

func streamRequest(t *testing.T, h http.Handler, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// newStreamServer wires a Server with the given StreamFunc, exercising the real
// HTTP boundary (decode/route/Visit) exactly as production.
func newStreamServer(t *testing.T, router *proxy.Router, stream server.StreamFunc) http.Handler {
	t.Helper()
	hh := health.New(discardLogger(), 0)
	srv := server.New(router, hh, discardLogger(), server.WithStreamFunc(stream))
	return srv.Handler(http.NewServeMux())
}

// TestStream_BreakerOpen_FastFail503 covers AC-014a: with the per-provider
// breaker OPEN, a stream request fast-fails 503 + Retry-After with NO 200/SSE
// bytes, and InferStream is NOT called (initiation is short-circuited).
func TestStream_BreakerOpen_FastFail503(t *testing.T) {
	t.Parallel()

	spy := &streamInitSpy{}
	router := proxy.NewRouter()
	router.Register("mock", spy)

	breakers := breaker.NewRegistry(config.Breaker{RetryAfter: 30 * time.Second},
		breaker.WithSettings(tripImmediately))
	composer := resilience.New(retry.New(config.Retry{}), breakers, 30*time.Second)
	streamFn := composer.StreamFunc()

	// Drive the breaker OPEN with a single failed initiation against a separate
	// failing provider, so the fast-fail test below can prove initiation is not
	// re-attempted against the real spy.
	failing := &streamInitSpy{failInit: provider.NewStatusError(503, "down")}
	_, _ = streamFn(context.Background(), failing, provider.Request{Model: "mock"})
	if breakers.State("mock") != gobreaker.StateOpen {
		t.Fatalf("expected breaker open, got %v", breakers.State("mock"))
	}

	h := newStreamServer(t, router, streamFn)
	rec := streamRequest(t, h, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Errorf("missing Retry-After header on 503")
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, must NOT be an SSE stream on fast-fail", ct)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Errorf("no SSE bytes must be written on fast-fail, body=%q", rec.Body.String())
	}
	if spy.initCalls.Load() != 0 {
		t.Errorf("InferStream must NOT be called on open breaker, got %d", spy.initCalls.Load())
	}
}

// TestStream_PoolSaturated_Returns503 covers AC-014b: with the worker pool
// saturated, a stream request returns 503 and no new stream is started; the
// concurrent slot count (including streams) never exceeds the limit.
func TestStream_PoolSaturated_Returns503(t *testing.T) {
	t.Parallel()

	const limit = 2
	p := pool.New(limit, 15*time.Second)

	// A held provider: each initiated stream emits one chunk then blocks until
	// the test releases it, so we can pin `limit` streams in flight.
	release := make(chan struct{})
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	held := func(ctx context.Context, _ provider.Provider, _ provider.Request) (<-chan provider.Chunk, error) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		out := make(chan provider.Chunk)
		go func() {
			defer close(out)
			defer inFlight.Add(-1)
			select {
			case out <- provider.Chunk{Content: "x"}:
			case <-ctx.Done():
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}()
		return out, nil
	}
	guarded := p.GuardStream(held)

	// Saturate: start `limit` streams and read their first chunk so each holds a
	// slot (the provider goroutine is then parked on `release`).
	var wg sync.WaitGroup
	for i := 0; i < limit; i++ {
		ch, err := guarded(context.Background(), nil, provider.Request{Model: "mock"})
		if err != nil {
			t.Fatalf("stream %d should acquire a slot, got %v", i, err)
		}
		<-ch // consume first chunk: the slot is now held for the stream lifetime.
		wg.Add(1)
		go func(c <-chan provider.Chunk) {
			defer wg.Done()
			for range c { // drain to close once released.
			}
		}(ch)
	}

	if got := p.InFlight(); got != limit {
		t.Fatalf("InFlight = %d, want %d (streams saturate the pool)", got, limit)
	}

	// The next stream request must 503 without starting a stream.
	_, err := guarded(context.Background(), nil, provider.Request{Model: "mock"})
	if !errors.Is(err, server.ErrServiceUnavailable) {
		t.Fatalf("saturated stream err = %v, want ErrServiceUnavailable", err)
	}

	// Also assert the server maps it to a 503 over the HTTP boundary.
	spy := &streamInitSpy{}
	router := proxy.NewRouter()
	router.Register("mock", spy)
	h := newStreamServer(t, router, guarded)
	rec := streamRequest(t, h, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.initCalls.Load() != 0 {
		t.Errorf("no stream may start when pool is saturated, got %d inits", spy.initCalls.Load())
	}

	// Release the held streams and assert the slot count never exceeded the limit
	// and drains back to zero.
	close(release)
	wg.Wait()
	if got := maxInFlight.Load(); int(got) > limit {
		t.Errorf("max concurrent streams = %d, exceeds limit %d", got, limit)
	}
	waitInFlightZero(t, p, time.Second)
}

// TestStream_RecordsProviderMetricAndSpan covers AC-014c: a successful stream
// observes provider_request_duration_seconds{provider} and records a provider
// OTel span, giving streaming the same metric/trace coverage as unary.
func TestStream_RecordsProviderMetricAndSpan(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	met := metrics.New(reg)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tracer := tracing.NewWithProvider(tp, tp.Shutdown)

	spy := &streamInitSpy{chunks: []provider.Chunk{{Content: "a"}, {Content: "b"}}}
	router := proxy.NewRouter()
	router.Register("mock", spy)

	defaultStream := func(ctx context.Context, p provider.Provider, req provider.Request) (<-chan provider.Chunk, error) {
		return p.InferStream(ctx, req)
	}
	streamFn := tracer.InstrumentStreamFunc(met.InstrumentStreamFunc(defaultStream))

	h := newStreamServer(t, router, streamFn)
	rec := streamRequest(t, h, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("expected a completed stream with [DONE], body=%q", rec.Body.String())
	}

	// Provider metric: the histogram for {provider="mock"} must have observed one
	// stream. Poll briefly because the observation fires from the forwarding
	// goroutine on channel close.
	waitMetricObserved(t, reg, "provider_request_duration_seconds", "mock", time.Second)

	// Provider span: at least one span named "provider.stream" must be recorded.
	waitSpanRecorded(t, sr, "provider.stream", time.Second)
}

// TestStream_PoolSlotReleasedOnEndAndCancel covers AC-014d: the pool slot is
// released exactly once on normal completion AND on mid-stream cancel; N
// sequential streams never exhaust the pool (InFlight back to 0 after each).
func TestStream_PoolSlotReleasedOnEndAndCancel(t *testing.T) {
	t.Parallel()

	const limit = 1 // a single slot magnifies any leak: the next stream would 503.
	p := pool.New(limit, time.Second)

	// gated provider: emits chunks with a per-chunk gate so a cancel test can stop
	// mid-stream; a normal completion test lets it run to the terminal chunk.
	makeProvider := func(perChunk time.Duration, n int) server.StreamFunc {
		return func(ctx context.Context, _ provider.Provider, _ provider.Request) (<-chan provider.Chunk, error) {
			out := make(chan provider.Chunk)
			go func() {
				defer close(out)
				for i := 0; i < n; i++ {
					if perChunk > 0 {
						tmr := time.NewTimer(perChunk)
						select {
						case <-ctx.Done():
							tmr.Stop()
							return
						case <-tmr.C:
						}
					}
					select {
					case out <- provider.Chunk{Content: "x"}:
					case <-ctx.Done():
						return
					}
				}
				select {
				case out <- provider.Chunk{Done: true}:
				case <-ctx.Done():
				}
			}()
			return out, nil
		}
	}

	// 1) Several NORMAL completions in a row must not exhaust the single slot.
	guardNormal := p.GuardStream(makeProvider(0, 3))
	for i := 0; i < 5; i++ {
		ch, err := guardNormal(context.Background(), nil, provider.Request{Model: "mock"})
		if err != nil {
			t.Fatalf("sequential stream %d should acquire slot, got %v (slot leaked?)", i, err)
		}
		for range ch { // drain to close.
		}
		waitInFlightZero(t, p, time.Second)
	}

	// 2) A MID-STREAM cancel must release the slot too.
	guardCancel := p.GuardStream(makeProvider(50*time.Millisecond, 100))
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := guardCancel(ctx, nil, provider.Request{Model: "mock"})
	if err != nil {
		t.Fatalf("stream should acquire slot, got %v", err)
	}
	<-ch // receive one chunk so the stream is genuinely active.
	cancel()
	// Abandon the channel like the server does on client disconnect (do NOT drain
	// `ch` here): the guard must still release the slot exactly once.
	waitInFlightZero(t, p, time.Second)

	// The slot is free again: a fresh stream must acquire it (proves no leak).
	ch2, err := guardNormal(context.Background(), nil, provider.Request{Model: "mock"})
	if err != nil {
		t.Fatalf("post-cancel stream should acquire slot, got %v (slot leaked on cancel)", err)
	}
	for range ch2 {
	}
	waitInFlightZero(t, p, time.Second)
}

// --- helpers ---------------------------------------------------------------

func waitInFlightZero(t *testing.T, p *pool.Pool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.InFlight() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pool.InFlight = %d, want 0 (slot leak)", p.InFlight())
}

func waitMetricObserved(t *testing.T, reg *prometheus.Registry, name, label string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather metrics: %v", err)
		}
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "provider" && lp.GetValue() == label {
						if m.GetHistogram().GetSampleCount() >= 1 {
							return
						}
					}
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("metric %s{provider=%q} not observed within %s", name, label, timeout)
}

func waitSpanRecorded(t *testing.T, sr *tracetest.SpanRecorder, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, s := range sr.Ended() {
			if s.Name() == name {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("span %q not recorded within %s", name, timeout)
}
