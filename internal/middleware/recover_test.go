package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adverax/sluice/internal/logging"
	"github.com/adverax/sluice/internal/middleware"
)

// syncBuffer is a goroutine-safe bytes.Buffer so a SafeGo goroutine can write
// log records while the test reads them, with no data race (the panic recovery
// path logs from a DIFFERENT goroutine than the test).
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

// captureLogger returns a logger writing JSON to buf so the test can assert the
// ERROR level + panic_value field (AC-033 / the AC-041 contract reused here).
func captureLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestPanicRecovery_Returns500AndContinues covers AC-033: a handler that panics
// yields a 500 to the client, the panic is logged at ERROR with panic_value, and
// the SAME handler chain serves the next request normally (process survives).
func TestPanicRecovery_Returns500AndContinues(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	logger := captureLogger(&buf)

	// Two distinct handlers (panicking / healthy) avoid sharing a flag across the
	// request goroutines; the same handler CHAIN is reused so the test proves the
	// process/chain survives a panic.
	panicHandler := middleware.Recoverer(logger)(logging.Middleware(logger)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("test panic")
		}),
	))
	okHandler := middleware.Recoverer(logger)(logging.Middleware(logger)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}),
	))

	// First request panics -> 500.
	rec1 := httptest.NewRecorder()
	panicHandler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec1.Code != http.StatusInternalServerError {
		t.Fatalf("panicking request: got status %d, want 500", rec1.Code)
	}

	// The panic must be logged at ERROR with the panic_value field.
	assertPanicLoggedAtError(t, buf.String(), "test panic")

	// Second request must succeed -> process survived (AC-033).
	rec2 := httptest.NewRecorder()
	okHandler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("subsequent request: got status %d, want 200", rec2.Code)
	}
}

// TestPanicRecovery_SubgoroutinePanicHandled covers AC-034: a sub-goroutine
// detached by a handler and wrapped with SafeGo panics; the process does NOT
// terminate and the next request receives a valid response without hanging.
func TestPanicRecovery_SubgoroutinePanicHandled(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	logger := captureLogger(&buf)

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A detached goroutine cannot be recovered by the handler's defer (Go
		// semantics): an unrecovered panic in ANY goroutine crashes the process.
		// SafeGo installs its own recover so the process survives (AC-034).
		middleware.SafeGo(logger, func() {
			panic("subgoroutine panic")
		})
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
	})

	handler := middleware.Recoverer(logger)(logging.Middleware(logger)(final))

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first request: got status %d, want 202", rec1.Code)
	}

	// The SafeGo recover logs the panic from a different goroutine; poll the
	// (synchronised) buffer until the ERROR record appears.
	waitForLog(t, &buf, "subgoroutine panic", 2*time.Second)
	assertPanicLoggedAtError(t, buf.String(), "subgoroutine panic")

	// Next request must still get a valid response (process did not terminate).
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("subsequent request: got status %d, want 202", rec2.Code)
	}
}

// assertPanicLoggedAtError parses the captured JSON log lines and asserts at
// least one record is at ERROR level and carries the panic_value field matching
// want. This mirrors the AC-041 contract (TestLogging_PanicLoggedAtError) which
// the recovery middleware reuses via logging.LogPanic.
func assertPanicLoggedAtError(t *testing.T, logs, want string) {
	t.Helper()
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["level"] != "ERROR" {
			continue
		}
		if pv, ok := rec["panic_value"]; ok {
			if s, _ := pv.(string); strings.Contains(s, want) {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("expected an ERROR log record with panic_value containing %q; logs:\n%s", want, logs)
	}
}

// waitForLog polls buf until it contains want or the deadline elapses. Used to
// observe a log record emitted from a SafeGo goroutine without racing on the
// buffer.
func waitForLog(t *testing.T, buf *syncBuffer, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for log containing %q", want)
}
