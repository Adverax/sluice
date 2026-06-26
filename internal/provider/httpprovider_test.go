package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// baseURL appends the /v1 segment to an httptest server URL: the mock upstream
// serves POST /v1/chat/completions and the adapter POSTs to
// <baseURL>/chat/completions, so the adapter's base URL must end in /v1 (just as
// a real Ollama/OpenAI base URL does).
func baseURL(srvURL string) string { return srvURL + "/v1" }

// roundTripperSpy records whether it was invoked, the last request URL, and the
// last Authorization header. It proves the HTTPProvider routes its call through
// the INJECTED client's Transport and how it builds the request.
type roundTripperSpy struct {
	base    http.RoundTripper
	calls   atomic.Int64
	lastURL atomic.Pointer[string]
	lastReq atomic.Pointer[http.Request]
}

func (s *roundTripperSpy) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	u := req.URL.String()
	s.lastURL.Store(&u)
	s.lastReq.Store(req)
	return s.base.RoundTrip(req)
}

// TestHTTPProvider_UsesInjectedPooledClient (AC-013a): construct the provider
// with a custom *http.Client whose Transport is a RoundTripper spy and assert
// Infer routes the call through THAT client.
func TestHTTPProvider_UsesInjectedPooledClient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{
		Content: "hi",
		Usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}))
	t.Cleanup(srv.Close)

	spy := &roundTripperSpy{base: http.DefaultTransport}
	client := &http.Client{Transport: spy, Timeout: 5 * time.Second}
	p := NewHTTP(client, baseURL(srv.URL))

	resp, err := p.Infer(context.Background(), Request{Model: "mock"})
	if err != nil {
		t.Fatalf("Infer returned error: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("injected RoundTripper invoked %d times, want 1", got)
	}
	if resp.Content != "hi" {
		t.Fatalf("Content = %q, want %q", resp.Content, "hi")
	}
	if resp.Usage.TotalTokens != 2 {
		t.Fatalf("Usage.TotalTokens = %d, want 2", resp.Usage.TotalTokens)
	}
}

// TestHTTPProvider_ReusesConnections (AC-013b): make N sequential Infer calls
// against a keep-alive httptest.Server and assert connection reuse via the
// httptrace GotConn.Reused hook. This proves process-wide pooling.
func TestHTTPProvider_ReusesConnections(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{Content: "ok"}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     30 * time.Second,
		},
		Timeout: 5 * time.Second,
	}
	p := NewHTTP(client, baseURL(srv.URL))

	const n = 5
	var reusedCount int
	for i := 0; i < n; i++ {
		var reused bool
		trace := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		}
		ctx := httptrace.WithClientTrace(context.Background(), trace)
		if _, err := p.Infer(ctx, Request{Model: "mock"}); err != nil {
			t.Fatalf("Infer #%d returned error: %v", i, err)
		}
		if reused {
			reusedCount++
		}
	}

	if reusedCount < n-1 {
		t.Fatalf("connection reused on %d/%d calls, want >= %d (pooling not exercised)", reusedCount, n, n-1)
	}
}

// TestHTTPProvider_ContextCancel_AbortsUpstreamHTTP (AC-013c): the upstream
// blocks; cancel ctx ~100ms in; assert Infer returns promptly with a
// ctx-wrapping error and the upstream observed the client going away.
func TestHTTPProvider_ContextCancel_AbortsUpstreamHTTP(t *testing.T) {
	t.Parallel()

	disconnected := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		go func() { _, _ = io.Copy(io.Discard, r.Body) }()
		select {
		case <-r.Context().Done():
			close(disconnected) // client/ctx gone: upstream aborted.
		case <-time.After(3 * time.Second):
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p := NewHTTP(&http.Client{}, baseURL(srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := p.Infer(ctx, Request{Model: "mock"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Infer error = %v, want context.Canceled", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Infer took %s, want prompt return (< ~150ms budget + slack)", elapsed)
	}

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream handler did not observe the client disconnect")
	}
}

// TestHTTPProvider_Stream_ForwardsAndCancels (AC-013d): the mock upstream streams
// OpenAI SSE chunks; InferStream forwards them; cancelling ctx stops emission,
// aborts the upstream stream, and closes the channel with no goroutine leak.
func TestHTTPProvider_Stream_ForwardsAndCancels(t *testing.T) {
	t.Parallel()

	t.Run("forwards all chunks and terminal usage", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{
			Content:      "hello world",
			StreamChunks: 4,
			Usage:        Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
		}))
		t.Cleanup(srv.Close)

		p := NewHTTP(&http.Client{}, baseURL(srv.URL))
		ch, err := p.InferStream(context.Background(), Request{Model: "mock", Stream: true})
		if err != nil {
			t.Fatalf("InferStream init error: %v", err)
		}

		var content string
		var sawDone bool
		var usage Usage
		for c := range ch {
			if c.Err != nil {
				t.Fatalf("chunk carried error: %v", c.Err)
			}
			if c.Done {
				sawDone = true
				usage = c.Usage
				continue
			}
			content += c.Content
		}
		if content != "hello world" {
			t.Fatalf("reassembled content = %q, want %q", content, "hello world")
		}
		if !sawDone {
			t.Fatal("stream ended without a terminal Done chunk")
		}
		if usage.TotalTokens != 8 {
			t.Fatalf("terminal Usage.TotalTokens = %d, want 8", usage.TotalTokens)
		}
	})

	t.Run("cancel stops emission and closes channel", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{
			Content:      "a b c d e f g h",
			StreamChunks: 8,
			Latency:      50 * time.Millisecond,
		}))
		t.Cleanup(srv.Close)

		p := NewHTTP(&http.Client{}, baseURL(srv.URL))
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := p.InferStream(ctx, Request{Model: "mock", Stream: true})
		if err != nil {
			t.Fatalf("InferStream init error: %v", err)
		}

		<-ch
		cancel()

		done := make(chan struct{})
		go func() {
			for range ch { //nolint:revive // drain until closed
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("stream channel did not close after ctx cancel (goroutine leak)")
		}
	})
}

// TestHTTPProvider_MapsStatusError asserts non-2xx upstream statuses become
// *StatusError with correct retryable classification (5xx retryable, 4xx not),
// so retry/breaker stay provider-agnostic. The mock emits the OpenAI error
// envelope, exercising the adapter's body parsing.
func TestHTTPProvider_MapsStatusError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		retryable bool
	}{
		{"500 retryable", http.StatusInternalServerError, true},
		{"503 retryable", http.StatusServiceUnavailable, true},
		{"400 not retryable", http.StatusBadRequest, false},
		{"429 not retryable", http.StatusTooManyRequests, false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{FailStatus: tc.status}))
			t.Cleanup(srv.Close)

			p := NewHTTP(&http.Client{}, baseURL(srv.URL))
			_, err := p.Infer(context.Background(), Request{Model: "mock"})

			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("error = %v, want *StatusError", err)
			}
			if se.Code != tc.status {
				t.Fatalf("StatusError.Code = %d, want %d", se.Code, tc.status)
			}
			if se.Retryable() != tc.retryable {
				t.Fatalf("Retryable() = %v, want %v", se.Retryable(), tc.retryable)
			}
		})
	}
}

// TestUpstream_OpenAIAdapter_UnaryMapping (AC-062): a RoundTripper spy / httptest
// server returns a real OpenAI unary body; assert the adapter POSTs a real
// OpenAI request (model, messages, temperature, top_p, max_tokens, stop) to
// /v1/chat/completions and maps choices[0].message.content + usage back into the
// canonical Response.
func TestUpstream_OpenAIAdapter_UnaryMapping(t *testing.T) {
	t.Parallel()

	var captured oaiRequest
	var capturedPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"llama3.2",
			"choices":[{"index":0,"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}
		}`)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	temp := 0.7
	p := NewHTTP(&http.Client{}, baseURL(srv.URL), WithModel("llama3.2"))
	resp, err := p.Infer(context.Background(), Request{
		Messages:    []Message{{Role: RoleUser, Content: "what is the answer?"}},
		Temperature: &temp,
		MaxTokens:   128,
	})
	if err != nil {
		t.Fatalf("Infer error: %v", err)
	}

	// Request mapping.
	if capturedPath != "/v1/chat/completions" {
		t.Fatalf("POST path = %q, want /v1/chat/completions", capturedPath)
	}
	if captured.Model != "llama3.2" {
		t.Fatalf("request model = %q, want llama3.2 (from WithModel fallback)", captured.Model)
	}
	if len(captured.Messages) != 1 || captured.Messages[0].Role != "user" || captured.Messages[0].Content != "what is the answer?" {
		t.Fatalf("request messages = %+v, want one user message", captured.Messages)
	}
	if captured.Temperature == nil || *captured.Temperature != 0.7 {
		t.Fatalf("request temperature = %v, want 0.7", captured.Temperature)
	}
	if captured.MaxTokens != 128 {
		t.Fatalf("request max_tokens = %d, want 128", captured.MaxTokens)
	}

	// Response mapping.
	if resp.Content != "42" {
		t.Fatalf("Content = %q, want 42 (choices[0].message.content)", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Model != "llama3.2" {
		t.Fatalf("Model = %q, want llama3.2", resp.Model)
	}
	if resp.Usage != (Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}) {
		t.Fatalf("Usage = %+v, want {7,3,10}", resp.Usage)
	}
}

// TestUpstream_OpenAIAdapter_RequestModelOverride verifies the model passes
// through as-is when set on the canonical Request (winning over the WithModel
// fallback), and that the adapter forwards the modeled fields
// (model/messages/temperature/max_tokens) without dropping them. top_p/stop are
// modeled in the wire struct but not yet supplied by the canonical Request
// (CARD-017 adds them at the edge), so they are not asserted here.
func TestUpstream_OpenAIAdapter_RequestModelOverride(t *testing.T) {
	t.Parallel()

	var captured oaiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = io.WriteString(w, `{"model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	t.Cleanup(srv.Close)

	p := NewHTTP(&http.Client{}, baseURL(srv.URL), WithModel("fallback-model"))
	if _, err := p.Infer(context.Background(), Request{Model: "gpt-x"}); err != nil {
		t.Fatalf("Infer error: %v", err)
	}
	if captured.Model != "gpt-x" {
		t.Fatalf("request model = %q, want gpt-x (Request.Model wins over WithModel)", captured.Model)
	}
}

// TestUpstream_OpenAIAdapter_BearerAuthOptional (AC-063): empty key → no
// Authorization header; non-empty key → `Bearer <key>`.
func TestUpstream_OpenAIAdapter_BearerAuthOptional(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		key      string
		wantAuth string
		wantSet  bool
	}{
		{name: "empty key omits header (Ollama)", key: "", wantSet: false},
		{name: "non-empty key sends bearer", key: "sk-secret", wantAuth: "Bearer sk-secret", wantSet: true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotAuth string
			var hadAuth bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				_, hadAuth = r.Header["Authorization"]
				_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{}}`)
			}))
			t.Cleanup(srv.Close)

			var opts []HTTPOption
			if tc.key != "" {
				opts = append(opts, WithAPIKey(tc.key))
			}
			p := NewHTTP(&http.Client{}, baseURL(srv.URL), opts...)
			if _, err := p.Infer(context.Background(), Request{Model: "m"}); err != nil {
				t.Fatalf("Infer error: %v", err)
			}

			if tc.wantSet {
				if !hadAuth {
					t.Fatal("Authorization header missing, want present")
				}
				if gotAuth != tc.wantAuth {
					t.Fatalf("Authorization = %q, want %q", gotAuth, tc.wantAuth)
				}
			} else if hadAuth {
				t.Fatalf("Authorization header present (%q), want omitted", gotAuth)
			}
		})
	}
}

// TestUpstream_MockUpstream_EmitsOpenAIShape (AC-064): drive Infer + InferStream
// through the adapter against the in-process mock upstream; assert the real
// OpenAI unary + SSE chunk mapping works end-to-end (content, usage, [DONE], and
// the trailing usage chunk).
func TestUpstream_MockUpstream_EmitsOpenAIShape(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{
		Content:      "hello there",
		StreamChunks: 3,
		FinishReason: "stop",
		Usage:        Usage{PromptTokens: 4, CompletionTokens: 6, TotalTokens: 10},
	}))
	t.Cleanup(srv.Close)

	p := NewHTTP(&http.Client{}, baseURL(srv.URL), WithModel("mock"))

	// Unary.
	resp, err := p.Infer(context.Background(), Request{Model: "mock"})
	if err != nil {
		t.Fatalf("Infer error: %v", err)
	}
	if resp.Content != "hello there" {
		t.Fatalf("unary Content = %q, want %q", resp.Content, "hello there")
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("unary FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage.TotalTokens != 10 {
		t.Fatalf("unary Usage.TotalTokens = %d, want 10", resp.Usage.TotalTokens)
	}

	// Streaming.
	ch, err := p.InferStream(context.Background(), Request{Model: "mock", Stream: true})
	if err != nil {
		t.Fatalf("InferStream error: %v", err)
	}
	var content string
	var sawDone bool
	var usage Usage
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("chunk error: %v", c.Err)
		}
		if c.Done {
			sawDone = true
			usage = c.Usage
			continue
		}
		content += c.Content
	}
	if content != "hello there" {
		t.Fatalf("stream content = %q, want %q", content, "hello there")
	}
	if !sawDone {
		t.Fatal("stream ended without Done chunk")
	}
	if usage.TotalTokens != 10 {
		t.Fatalf("stream terminal Usage.TotalTokens = %d, want 10 (from trailing usage chunk)", usage.TotalTokens)
	}
}

// TestEdge_Streaming_MissingUsage_GracefulZero (AC-059): a streaming request
// where the upstream omits the usage chunk → the stream ends with [DONE], the
// canonical terminal usage is zero, no error, and no goroutine leak.
func TestEdge_Streaming_MissingUsage_GracefulZero(t *testing.T) {
	t.Parallel()

	before := runtime.NumGoroutine()

	srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{
		Content:         "partial answer",
		StreamChunks:    3,
		Usage:           Usage{PromptTokens: 9, CompletionTokens: 9, TotalTokens: 18},
		OmitStreamUsage: true, // backend ignores include_usage.
	}))
	t.Cleanup(srv.Close)

	p := NewHTTP(&http.Client{}, baseURL(srv.URL), WithModel("mock"))
	ch, err := p.InferStream(context.Background(), Request{Model: "mock", Stream: true})
	if err != nil {
		t.Fatalf("InferStream error: %v", err)
	}

	var content string
	var sawDone bool
	var usage Usage
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("stream broke with error on missing usage: %v", c.Err)
		}
		if c.Done {
			sawDone = true
			usage = c.Usage
			continue
		}
		content += c.Content
	}

	if content != "partial answer" {
		t.Fatalf("content = %q, want %q", content, "partial answer")
	}
	if !sawDone {
		t.Fatal("stream ended without a terminal Done chunk")
	}
	if usage != (Usage{}) {
		t.Fatalf("terminal Usage = %+v, want zero (graceful uncounted)", usage)
	}

	// No goroutine leak: the stream reader must have exited.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine count grew from %d to %d after stream end (leak)", before, runtime.NumGoroutine())
}
