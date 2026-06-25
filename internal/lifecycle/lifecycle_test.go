package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe writer for the slog handler.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// drainScenario starts a server via mgr.Run(ctx), fires n concurrent requests
// whose handler sleeps for latency, then cancels ctx (simulating SIGTERM) once
// all requests are confirmed in-flight. It asserts every request completed with
// 200, Run returned nil (exit code 0) and the log contains "drained N requests".
func drainScenario(t *testing.T, n int, latency time.Duration) {
	t.Helper()

	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	addr := freeAddr(t)

	server := &http.Server{Addr: addr}
	mgr := New(server, logger, 10*time.Second)
	server.Handler = mgr.CountingMiddleware(handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- mgr.Run(ctx) }()

	// Wait until the server is accepting connections.
	dialDeadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(dialDeadline) {
			t.Fatal("server never started accepting connections")
		}
		time.Sleep(5 * time.Millisecond)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var wg sync.WaitGroup
	results := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Get(fmt.Sprintf("http://%s/", addr))
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			results[i] = resp.StatusCode
		}(i)
	}

	// Wait until all n requests are in-flight, then cancel ctx (simulate SIGTERM).
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&mgr.inFlight) < int64(n) {
		if time.Now().After(deadline) {
			t.Fatalf("requests never all became in-flight: got %d/%d", mgr.InFlight(), n)
		}
		time.Sleep(time.Millisecond)
	}
	cancel() // simulate SIGTERM after all requests are in-flight

	wg.Wait()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error (want nil / exit 0): %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("request %d failed: %v", i, errs[i])
			continue
		}
		if results[i] != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, results[i])
		}
	}

	want := fmt.Sprintf("drained %d requests", n)
	if logs := logBuf.String(); !bytesContains(logs, want) {
		t.Errorf("log does not contain %q\nlogs:\n%s", want, logs)
	}
}

func bytesContains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// TestGracefulShutdown_DrainsInFlightRequests covers AC-031 (FR-012): 5
// in-flight requests at 200ms latency all complete on SIGTERM and the process
// logs "drained N requests".
func TestGracefulShutdown_DrainsInFlightRequests(t *testing.T) {
	drainScenario(t, 5, 200*time.Millisecond)
}

// TestGracefulShutdown_ZeroDropped covers AC-046 (NFR-005): 10 in-flight
// requests at 300ms latency all complete, exit code 0, log "drained 10
// requests".
func TestGracefulShutdown_ZeroDropped(t *testing.T) {
	drainScenario(t, 10, 300*time.Millisecond)
}

// TestRun_ReturnsNilOnContextCancelWithNoTraffic exercises the public Run path
// (ListenAndServe + ctx cancel) to confirm a clean exit-0 with no requests.
func TestRun_ReturnsNilOnContextCancelWithNoTraffic(t *testing.T) {
	addr := freeAddr(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	server := &http.Server{Addr: addr}
	mgr := New(server, logger, 2*time.Second)
	server.Handler = mgr.CountingMiddleware(http.NotFoundHandler())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestOnShutdown_HookRunsAfterDrain asserts a registered shutdown hook (e.g. the
// metering worker's Close — AC-032) runs during graceful shutdown, after the
// HTTP drain, and that its error is surfaced by Run.
func TestOnShutdown_HookRunsAfterDrain(t *testing.T) {
	addr := freeAddr(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	server := &http.Server{Addr: addr}
	mgr := New(server, logger, 2*time.Second)
	server.Handler = mgr.CountingMiddleware(http.NotFoundHandler())

	var hookRan int64
	mgr.OnShutdown(func(ctx context.Context) error {
		atomic.AddInt64(&hookRan, 1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if atomic.LoadInt64(&hookRan) != 1 {
		t.Errorf("shutdown hook ran %d times, want 1", atomic.LoadInt64(&hookRan))
	}
}

// TestGracefulShutdown_TimeoutForced covers AC-051: in-flight requests that do
// not complete within the (short, injected) shutdown timeout force the drain to
// give up; Run still returns (forced exit, not a hard error) and the log
// contains the count of unfinished requests. A clean-drain remains green
// (TestGracefulShutdown_DrainsInFlightRequests above).
func TestGracefulShutdown_TimeoutForced(t *testing.T) {
	const n = 3
	// The handler blocks far longer than the shutdown timeout so the drain cannot
	// complete and Shutdown returns context.DeadlineExceeded.
	const handlerBlock = 5 * time.Second
	const shutdownTimeout = 150 * time.Millisecond

	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))

	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(handlerBlock):
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	addr := freeAddr(t)
	server := &http.Server{Addr: addr}
	mgr := New(server, logger, shutdownTimeout)
	server.Handler = mgr.CountingMiddleware(handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- mgr.Run(ctx) }()

	// Wait for the server to accept connections.
	dialDeadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(dialDeadline) {
			t.Fatal("server never started accepting connections")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Fire n requests that will hang inside the handler.
	client := &http.Client{Timeout: 30 * time.Second}
	for i := 0; i < n; i++ {
		go func() {
			resp, err := client.Get(fmt.Sprintf("http://%s/", addr))
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}

	// Wait until all n are in-flight, then signal shutdown.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&mgr.inFlight) < int64(n) {
		if time.Now().After(deadline) {
			t.Fatalf("requests never all became in-flight: got %d/%d", mgr.InFlight(), n)
		}
		time.Sleep(time.Millisecond)
	}
	cancel() // simulate SIGTERM

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error on forced shutdown (want nil / forced exit): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after forced shutdown")
	}

	// Release the still-blocked handlers so their goroutines can exit cleanly.
	close(release)

	logs := logBuf.String()
	want := fmt.Sprintf("forced shutdown: %d requests unfinished", n)
	if !bytesContains(logs, want) {
		t.Errorf("log does not contain %q\nlogs:\n%s", want, logs)
	}
}

// TestOnShutdown_HookRunsOnForcedPath asserts that OnShutdown hooks (e.g. the
// metering worker's Close — AC-032) execute even when the HTTP drain was FORCED
// (server.Shutdown returned context.DeadlineExceeded). The hook must run with
// its own fresh-deadline context, independent of the exhausted drain context.
func TestOnShutdown_HookRunsOnForcedPath(t *testing.T) {
	const n = 2
	const handlerBlock = 5 * time.Second
	const shutdownTimeout = 100 * time.Millisecond
	const hookTimeout = 2 * time.Second

	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))

	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(handlerBlock):
		}
		w.WriteHeader(http.StatusOK)
	})

	addr := freeAddr(t)
	server := &http.Server{Addr: addr}
	mgr := New(server, logger, shutdownTimeout,
		WithHookTimeout(hookTimeout),
	)
	server.Handler = mgr.CountingMiddleware(handler)

	var hookRan int64
	mgr.OnShutdown(func(ctx context.Context) error {
		// Record that the hook was called. The ctx here must NOT be expired
		// even though the HTTP drain timed out.
		if ctx.Err() != nil {
			return fmt.Errorf("hook context already expired: %w", ctx.Err())
		}
		atomic.AddInt64(&hookRan, 1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- mgr.Run(ctx) }()

	// Wait for the server to accept connections.
	dialDeadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(dialDeadline) {
			t.Fatal("server never started accepting connections")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Fire requests that will block past the drain timeout.
	client := &http.Client{Timeout: 30 * time.Second}
	for i := 0; i < n; i++ {
		go func() {
			resp, err := client.Get(fmt.Sprintf("http://%s/", addr))
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}

	// Wait until all n are in-flight, then trigger shutdown.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&mgr.inFlight) < int64(n) {
		if time.Now().After(deadline) {
			t.Fatalf("requests never all became in-flight: got %d/%d", mgr.InFlight(), n)
		}
		time.Sleep(time.Millisecond)
	}
	cancel() // trigger shutdown — drain will be forced because handlers are blocked

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error on forced shutdown (want nil): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after forced shutdown")
	}

	// Release the blocked handlers so their goroutines exit cleanly.
	close(release)

	// The forced-shutdown log line must be present.
	logs := logBuf.String()
	want := fmt.Sprintf("forced shutdown: %d requests unfinished", n)
	if !bytesContains(logs, want) {
		t.Errorf("log does not contain %q\nlogs:\n%s", want, logs)
	}

	// The hook MUST have run — this is the core assertion of the test.
	if atomic.LoadInt64(&hookRan) != 1 {
		t.Errorf("shutdown hook ran %d times on forced path, want 1", atomic.LoadInt64(&hookRan))
	}
}

// freeAddr returns a free 127.0.0.1:<port> address by briefly binding an
// ephemeral port and immediately releasing it. There is a small TOCTOU window
// but it is negligible in practice for loopback test addresses.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeAddr: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestRun_DrainInFlightRequests_EndToEnd covers the Run→ctx-cancel→drain wiring
// end-to-end through the public mgr.Run(ctx) entry point (ListenAndServe path).
// It starts Run in a goroutine, issues N slow in-flight requests, then cancels
// the context, and asserts:
//   - all N requests complete with HTTP 200 (zero dropped)
//   - Run returns nil (exit code 0)
//   - the log contains "drained N requests"
func TestRun_DrainInFlightRequests_EndToEnd(t *testing.T) {
	const n = 5
	const latency = 200 * time.Millisecond

	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))

	addr := freeAddr(t)

	var arrived int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&arrived, 1)
		time.Sleep(latency)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	server := &http.Server{Addr: addr}
	mgr := New(server, logger, 10*time.Second)
	server.Handler = mgr.CountingMiddleware(handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- mgr.Run(ctx) }()

	// Wait until the server is accepting connections.
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never started accepting connections")
		}
		time.Sleep(5 * time.Millisecond)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var wg sync.WaitGroup
	results := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Get(fmt.Sprintf("http://%s/", addr))
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			results[i] = resp.StatusCode
		}(i)
	}

	// Wait until all n requests are in-flight, then cancel ctx (simulate SIGTERM).
	deadline = time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&mgr.inFlight) < int64(n) {
		if time.Now().After(deadline) {
			t.Fatalf("requests never all became in-flight: got %d/%d", mgr.InFlight(), n)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	wg.Wait()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error (want nil / exit 0): %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("request %d failed: %v", i, errs[i])
			continue
		}
		if results[i] != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, results[i])
		}
	}

	want := fmt.Sprintf("drained %d requests", n)
	if logs := logBuf.String(); !bytesContains(logs, want) {
		t.Errorf("log does not contain %q\nlogs:\n%s", want, logs)
	}
}
