package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adverax/sluice/internal/api"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/metering"
	"github.com/adverax/sluice/internal/provider"
	"github.com/adverax/sluice/internal/proxy"
	"github.com/adverax/sluice/internal/server"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// spyProvider records whether Infer was called so negative-path tests can prove
// the provider is NOT contacted (AC-004/006/007). It can also be configured to
// return a canonical response or an error.
type spyProvider struct {
	resp    provider.Response
	err     error
	called  bool
	lastReq provider.Request // the canonical request the edge forwarded (AC-053/054)
}

func (s *spyProvider) Infer(_ context.Context, req provider.Request) (provider.Response, error) {
	s.called = true
	s.lastReq = req
	if s.err != nil {
		return provider.Response{}, s.err
	}
	return s.resp, nil
}

func (s *spyProvider) InferStream(context.Context, provider.Request) (<-chan provider.Chunk, error) {
	return nil, errors.New("not implemented in test")
}

// newTestServer builds the full generated HTTP boundary (api.HandlerFromMux +
// strict handler) around a Server, so tests exercise decode/route/map exactly
// as production does (ADR-0011). Extra checkers are registered on the health
// handler for readiness tests.
func newTestServer(t *testing.T, router *proxy.Router, checkers ...health.Checker) http.Handler {
	t.Helper()
	hh := health.New(discardLogger(), 0)
	hh.Register(checkers...)
	srv := server.New(router, hh, discardLogger())
	return srv.Handler(http.NewServeMux())
}

func doJSON(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestProxy_HappyPath_NonStreaming covers AC-001: a valid request to a
// registered provider returns 200 with the mapped provider response body.
func TestProxy_HappyPath_NonStreaming(t *testing.T) {
	spy := &spyProvider{resp: provider.Response{
		Model:        "gpt-4",
		Content:      "hello world",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !spy.called {
		t.Error("provider.Infer was not called")
	}
	var got api.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Object != "chat.completion" || got.Model != "gpt-4" {
		t.Errorf("unexpected response envelope: %+v", got)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Message.Content != "hello world" || c.Message.Role != "assistant" {
		t.Errorf("unexpected choice message: %+v", c.Message)
	}
	if c.FinishReason == nil || *c.FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", c.FinishReason)
	}
	if got.Usage.TotalTokens != 5 {
		t.Errorf("usage = %+v, want total 5", got.Usage)
	}
}

// captureSink is a test metering.Sink that records every enqueued event. It is
// goroutine-safe because the server may enqueue from the streaming Visit path.
type captureSink struct {
	mu     sync.Mutex
	events []metering.UsageEvent
}

func (c *captureSink) Enqueue(e metering.UsageEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureSink) snapshot() []metering.UsageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]metering.UsageEvent, len(c.events))
	copy(out, c.events)
	return out
}

// TestProxy_UnaryEnqueuesUsage asserts that after a successful non-streaming
// inference the server records a UsageEvent on the metering sink (FR-014) with
// the canonical usage and HTTP 200 status. This proves the metering wiring is in
// the hot path without changing the 200 response.
func TestProxy_UnaryEnqueuesUsage(t *testing.T) {
	spy := &spyProvider{resp: provider.Response{
		Model:        "gpt-4",
		Content:      "hello world",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)

	sink := &captureSink{}
	hh := health.New(discardLogger(), 0)
	srv := server.New(router, hh, discardLogger(), server.WithMeteringSink(sink))
	h := srv.Handler(http.NewServeMux())

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("enqueued %d usage events, want 1", len(events))
	}
	e := events[0]
	if e.Provider != "gpt-4" {
		t.Errorf("event provider = %q, want gpt-4", e.Provider)
	}
	if e.TotalTokens != 5 || e.PromptTokens != 3 || e.CompletionTokens != 2 {
		t.Errorf("event usage = %+v, want 3/2/5", e)
	}
	if e.Status != http.StatusOK {
		t.Errorf("event status = %d, want 200", e.Status)
	}
}

// TestProxy_ProviderError_NoUsageEnqueued asserts a failed inference does NOT
// record a usage event (only successful completions are metered).
func TestProxy_ProviderError_NoUsageEnqueued(t *testing.T) {
	spy := &spyProvider{err: errors.New("upstream boom")}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)

	sink := &captureSink{}
	hh := health.New(discardLogger(), 0)
	srv := server.New(router, hh, discardLogger(), server.WithMeteringSink(sink))
	h := srv.Handler(http.NewServeMux())

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("enqueued %d usage events on failure, want 0", got)
	}
}

// TestProxy_ProviderError_Returns502 covers AC-003: a provider failure maps to
// 502 with a JSON error body.
func TestProxy_ProviderError_Returns502(t *testing.T) {
	spy := &spyProvider{err: errors.New("upstream 500")}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var apiErr api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if apiErr.Error.Message == "" || apiErr.Error.Type == "" {
		t.Errorf("expected OpenAI error envelope, got %+v", apiErr)
	}
}

// TestProxy_InvalidBody_Returns400 covers AC-004: a malformed JSON body yields
// 400 without contacting the provider. The strict server handles the decode.
func TestProxy_InvalidBody_Returns400(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4", not valid json`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.called {
		t.Error("provider.Infer must not be called on a malformed body")
	}
}

// TestRouter_MissingModel_Returns400 covers AC-006: an absent model yields 400
// without contacting the provider.
func TestRouter_MissingModel_Returns400(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.called {
		t.Error("provider.Infer must not be called when model is absent")
	}
}

// TestRouter_UnknownModel_Returns404 covers AC-007: an unregistered model
// yields 404 without contacting any provider.
func TestRouter_UnknownModel_Returns404(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"unregistered","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.called {
		t.Error("provider.Infer must not be called for an unknown model")
	}
}

// TestRouter_RoutesToCorrectProvider covers AC-005 at the HTTP boundary: two
// providers registered for two models; the request routes to the one matching
// the model field.
func TestRouter_RoutesToCorrectProvider(t *testing.T) {
	gpt4 := &spyProvider{resp: provider.Response{Model: "gpt-4", Content: "from gpt-4"}}
	claude := &spyProvider{resp: provider.Response{Model: "claude-3", Content: "from claude"}}
	router := proxy.NewRouter()
	router.Register("gpt-4", gpt4)
	router.Register("claude-3", claude)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if gpt4.called {
		t.Error("gpt-4 provider must not be called for a claude-3 request")
	}
	if !claude.called {
		t.Error("claude-3 provider was not called")
	}
	var got api.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "from claude" {
		t.Errorf("routed to wrong provider: %+v", got.Choices)
	}
}

// TestProxy_InvalidRole_Returns400 covers ADR-0011 schema validation: a message
// with an unknown role value (not in the spec enum) must be rejected 400 by the
// OpenAPI request validator before reaching the provider. The spy MUST NOT be
// called.
func TestProxy_InvalidRole_Returns400(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"banana","content":"x"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.called {
		t.Error("provider.Infer must not be called when role is invalid")
	}
}

// TestProxy_EmptyMessages_Returns400 covers ADR-0011 schema validation: the
// messages field is required and must have at least one element. An empty
// messages array must be rejected 400 before reaching the provider.
func TestProxy_EmptyMessages_Returns400(t *testing.T) {
	spy := &spyProvider{}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.called {
		t.Error("provider.Infer must not be called when messages is empty")
	}
}

// --- Streaming path (FR-001 AC-002, FR-003 AC-008/AC-009) ------------------

// streamSpy is a streaming Provider test double whose chunk emission is gated by
// the test. It records when its background goroutine stops (so leak/abort can be
// asserted) and honours ctx at every step.
type streamSpy struct {
	chunks   []provider.Chunk // content chunks to emit before the terminal Done chunk
	perChunk time.Duration    // delay before each emit, honoured against ctx
	started  atomic.Bool
	stopped  atomic.Bool           // set when the goroutine returns
	ctxErr   atomic.Pointer[error] // the ctx error observed when aborting (if any)
}

func (s *streamSpy) Infer(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, errors.New("unary not used in streaming test")
}

func (s *streamSpy) InferStream(ctx context.Context, _ provider.Request) (<-chan provider.Chunk, error) {
	s.started.Store(true)
	out := make(chan provider.Chunk)
	go func() {
		defer close(out)
		defer s.stopped.Store(true)
		for _, c := range s.chunks {
			if s.perChunk > 0 {
				t := time.NewTimer(s.perChunk)
				select {
				case <-ctx.Done():
					t.Stop()
					err := ctx.Err()
					s.ctxErr.Store(&err)
					return
				case <-t.C:
				}
			}
			select {
			case out <- c:
			case <-ctx.Done():
				err := ctx.Err()
				s.ctxErr.Store(&err)
				return
			}
		}
		select {
		case out <- provider.Chunk{Done: true, Usage: provider.Usage{TotalTokens: 7}}:
		case <-ctx.Done():
			err := ctx.Err()
			s.ctxErr.Store(&err)
		}
	}()
	return out, nil
}

func countDataEvents(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && !strings.HasPrefix(line, "data: [DONE]") {
			n++
		}
	}
	return n
}

// TestProxy_HappyPath_Streaming covers AC-002: stream:true returns 200 with
// Content-Type text/event-stream and forwards the SSE deltas as they arrive.
func TestProxy_HappyPath_Streaming(t *testing.T) {
	mock := provider.New(
		provider.WithResponse(provider.Response{
			Model:   "gpt-4",
			Content: "hello streaming world",
			Usage:   provider.Usage{TotalTokens: 7},
		}),
		provider.WithStreamChunks(3),
	)
	router := proxy.NewRouter()
	router.Register("gpt-4", mock)
	h := newTestServer(t, router)

	rec := doJSON(t, h, `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if got := countDataEvents(body); got < 2 {
		t.Errorf("forwarded %d SSE data events, want >= 2 (body=%q)", got, body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("expected terminal [DONE] marker, body=%q", body)
	}
}

// TestProxy_ClientCancel_AbortsUpstream covers AC-008: a request is in flight
// with high upstream latency; the client cancels and the ctx passed to
// InferStream is cancelled, so the handler returns promptly and the provider
// stream goroutine stops (no leak).
func TestProxy_ClientCancel_AbortsUpstream(t *testing.T) {
	spy := &streamSpy{
		chunks:   []provider.Chunk{{Content: "a"}, {Content: "b"}, {Content: "c"}},
		perChunk: 500 * time.Millisecond,
	}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Cancel ~100ms in, while the first 500ms chunk is still pending.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("handler did not return promptly after client cancel")
	}

	// The provider stream goroutine must have observed cancellation and stopped.
	waitTrue(t, &spy.stopped, 200*time.Millisecond, "provider stream goroutine did not stop")
	if e := spy.ctxErr.Load(); e == nil || !errors.Is(*e, context.Canceled) {
		t.Errorf("upstream ctx error = %v, want context.Canceled", e)
	}
}

// TestProxy_StreamingClientCancel_AbortsUpstream covers AC-009: during an active
// SSE stream the client closes; the gateway stops forwarding and the upstream is
// cancelled, with no goroutine leak.
func TestProxy_StreamingClientCancel_AbortsUpstream(t *testing.T) {
	before := runtime.NumGoroutine()

	spy := &streamSpy{
		// Many chunks with a small per-chunk delay so the stream is genuinely
		// active (already forwarding) when the client cancels mid-stream.
		chunks:   make([]provider.Chunk, 50),
		perChunk: 20 * time.Millisecond,
	}
	for i := range spy.chunks {
		spy.chunks[i] = provider.Chunk{Content: "x"}
	}
	router := proxy.NewRouter()
	router.Register("gpt-4", spy)
	h := newTestServer(t, router)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Let several chunks flow, then close the connection mid-stream.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("handler did not stop forwarding promptly after mid-stream cancel")
	}

	waitTrue(t, &spy.stopped, 200*time.Millisecond, "provider stream goroutine leaked after cancel")
	if e := spy.ctxErr.Load(); e == nil || !errors.Is(*e, context.Canceled) {
		t.Errorf("upstream ctx error = %v, want context.Canceled", e)
	}

	// No goroutine leak: allow the scheduler a moment to reclaim, then compare
	// with a tolerance for test-harness background goroutines.
	waitGoroutines(t, before+2, 500*time.Millisecond)
}

// waitTrue polls an atomic.Bool until it is true or the deadline elapses.
func waitTrue(t *testing.T, b *atomic.Bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// waitGoroutines polls until the goroutine count drops to <= limit or times out.
func waitGoroutines(t *testing.T, limit int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= limit {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine count = %d, want <= %d (possible leak)", runtime.NumGoroutine(), limit)
}

// --- Health & readiness (FR-008/FR-009) -----------------------------------

func getReadyz(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec
}

func okChecker(name string) health.Checker {
	return health.CheckerFunc{CheckerName: name, CheckFunc: func(context.Context) error { return nil }}
}

func downChecker(name string) health.Checker {
	return health.CheckerFunc{CheckerName: name, CheckFunc: func(context.Context) error {
		return errors.New("down")
	}}
}

// TestHealthz_ReturnsOK covers AC-025: /healthz returns 200 with status ok.
func TestHealthz_ReturnsOK(t *testing.T) {
	h := newTestServer(t, proxy.NewRouter())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var hs api.HealthStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &hs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if hs.Status != "ok" {
		t.Errorf("status field = %q, want ok", hs.Status)
	}
}

// TestReadyz_AllDepsUp_Returns200 covers AC-026: both deps healthy → 200 with a
// per-dependency body.
func TestReadyz_AllDepsUp_Returns200(t *testing.T) {
	h := newTestServer(t, proxy.NewRouter(), okChecker("redis"), okChecker("postgres"))
	rec := getReadyz(t, h)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var rs api.ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rs.Dependencies["redis"] != "ok" || rs.Dependencies["postgres"] != "ok" {
		t.Errorf("dependencies = %+v, want both ok", rs.Dependencies)
	}
}

// TestReadyz_RedisDown_Returns503 covers AC-027: redis down → 503 with redis
// reported as not "ok" in the body.
func TestReadyz_RedisDown_Returns503(t *testing.T) {
	h := newTestServer(t, proxy.NewRouter(), downChecker("redis"), okChecker("postgres"))
	rec := getReadyz(t, h)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	var rs api.ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rs.Dependencies["redis"] == "ok" || rs.Dependencies["redis"] == "" {
		t.Errorf("redis dependency = %q, want a down reason", rs.Dependencies["redis"])
	}
	if rs.Dependencies["postgres"] != "ok" {
		t.Errorf("postgres dependency = %q, want ok", rs.Dependencies["postgres"])
	}
}

// TestReadyz_PostgresDown_Returns503 covers AC-028: postgres down → 503 with
// postgres reported as not "ok" in the body.
func TestReadyz_PostgresDown_Returns503(t *testing.T) {
	h := newTestServer(t, proxy.NewRouter(), okChecker("redis"), downChecker("postgres"))
	rec := getReadyz(t, h)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	var rs api.ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rs.Dependencies["postgres"] == "ok" || rs.Dependencies["postgres"] == "" {
		t.Errorf("postgres dependency = %q, want a down reason", rs.Dependencies["postgres"])
	}
}
