package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripperSpy records whether it was invoked and delegates to a base
// RoundTripper. It proves the HTTPProvider routes its call through the INJECTED
// client's Transport rather than a package-global one.
type roundTripperSpy struct {
	base    http.RoundTripper
	calls   atomic.Int64
	lastURL atomic.Pointer[string]
}

func (s *roundTripperSpy) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	u := req.URL.String()
	s.lastURL.Store(&u)
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
	p := NewHTTP(client, srv.URL)

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

	// A dedicated transport with idle-conn pooling enabled (mirrors the tuned
	// production client). The same client is reused across calls.
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     30 * time.Second,
		},
		Timeout: 5 * time.Second,
	}
	p := NewHTTP(client, srv.URL)

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

	// The first call dials a fresh connection; every subsequent call must reuse
	// the pooled one (drainAndClose returns it to the pool).
	if reusedCount < n-1 {
		t.Fatalf("connection reused on %d/%d calls, want >= %d (pooling not exercised)", reusedCount, n, n-1)
	}
}

// TestHTTPProvider_ContextCancel_AbortsUpstreamHTTP (AC-013c): the mock upstream
// sleeps 500ms; cancel ctx ~100ms in; assert Infer returns promptly with a
// ctx-wrapping error and the upstream observed the client going away.
func TestHTTPProvider_ContextCancel_AbortsUpstreamHTTP(t *testing.T) {
	t.Parallel()

	// The handler simulates a slow upstream: it never writes a body, so the
	// client's read blocks until ctx-cancel aborts it (proving the in-flight
	// HTTP call is bound to ctx). To OBSERVE the client going away, the handler
	// drains the request body in a background goroutine: Go's server cancels
	// r.Context() when that connection read sees the peer close — the documented
	// disconnect-detection path. It signals via a channel and is bounded by a
	// safety timer so the test can never hang.
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

	p := NewHTTP(&http.Client{}, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	// Infer blocks reading the response, so the cancelled ctx aborts the
	// in-flight call; the prompt error proves the upstream HTTP call is bound to
	// ctx (FR-003).
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
		// upstream observed the client going away.
	case <-time.After(2 * time.Second):
		t.Fatal("upstream handler did not observe the client disconnect")
	}
}

// TestHTTPProvider_Stream_ForwardsAndCancels (AC-013d): the mock upstream streams
// SSE chunks; InferStream forwards them; cancelling ctx stops emission, aborts
// the upstream stream, and closes the channel with no goroutine leak.
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

		p := NewHTTP(&http.Client{}, srv.URL)
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
		// Per-delta latency makes the stream long enough to cancel mid-flight.
		srv := httptest.NewServer(MockUpstreamHandler(MockUpstreamOptions{
			Content:      "a b c d e f g h",
			StreamChunks: 8,
			Latency:      50 * time.Millisecond,
		}))
		t.Cleanup(srv.Close)

		p := NewHTTP(&http.Client{}, srv.URL)
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := p.InferStream(ctx, Request{Model: "mock", Stream: true})
		if err != nil {
			t.Fatalf("InferStream init error: %v", err)
		}

		// Consume one chunk, then cancel.
		<-ch
		cancel()

		// The channel must close promptly after cancel (no goroutine leak).
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
// so retry/breaker stay provider-agnostic.
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

			p := NewHTTP(&http.Client{}, srv.URL)
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
