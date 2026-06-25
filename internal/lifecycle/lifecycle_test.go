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

// drainScenario starts a server whose handler sleeps for latency, fires n
// concurrent requests, then cancels ctx (simulating SIGTERM) once all requests
// are confirmed in-flight. It asserts every request completed with 200, Run
// returned nil (exit code 0) and the log contains "drained N requests".
func drainScenario(t *testing.T, n int, latency time.Duration) {
	t.Helper()

	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))

	var arrived int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&arrived, 1)
		time.Sleep(latency)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	// Bind to an ephemeral port so the test is hermetic.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	server := &http.Server{Addr: addr}
	mgr := New(server, logger, 10*time.Second)
	server.Handler = mgr.CountingMiddleware(handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		// Serve on the pre-bound listener so we avoid a listen race.
		err := server.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			runErr <- err
			return
		}
		runErr <- nil
	}()

	// Replace Run's ListenAndServe path: we drive shutdown directly via ctx,
	// mirroring Run's select. Wait for ctx then shutdown.
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownDone <- mgr.shutdown()
	}()

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

	// Wait until all n requests are in-flight, then send "SIGTERM".
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&mgr.inFlight) < int64(n) {
		if time.Now().After(deadline) {
			t.Fatalf("requests never all became in-flight: got %d/%d", mgr.InFlight(), n)
		}
		time.Sleep(time.Millisecond)
	}
	cancel() // simulate SIGTERM after all requests are in-flight

	wg.Wait()

	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown returned error (want nil / exit 0): %v", err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("serve returned error: %v", err)
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
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	server := &http.Server{Addr: "127.0.0.1:0"}
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
