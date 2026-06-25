package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// syncBuffer is a goroutine-safe buffer so the slog handler (which may be
// written to concurrently) does not race the test reading it.
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

// TestLogging_StructuredFieldsPresent covers AC-040 (FR-016): a completed
// request produces an slog record carrying request_id, latency_ms and
// status_code.
func TestLogging_StructuredFieldsPresent(t *testing.T) {
	buf := &syncBuffer{}
	logger := New(buf, "json", "info")

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var record map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &record); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, buf.String())
	}

	for _, field := range []string{"request_id", "latency_ms", "status_code"} {
		if _, ok := record[field]; !ok {
			t.Errorf("log record missing field %q; got: %v", field, record)
		}
	}
	if got := record["status_code"]; got != float64(http.StatusCreated) {
		t.Errorf("status_code = %v, want %d", got, http.StatusCreated)
	}
	if record["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", record["level"])
	}
	if rid, _ := record["request_id"].(string); rid == "" {
		t.Errorf("request_id is empty")
	}
}

// TestLogging_PanicLoggedAtError covers AC-041 (FR-016): when a handler panics,
// the log record is at ERROR level and carries the panic_value field. The
// middleware re-raises the panic (recovery-as-500 is CARD-009), so the test
// recovers it itself.
func TestLogging_PanicLoggedAtError(t *testing.T) {
	buf := &syncBuffer{}
	logger := New(buf, "json", "info")

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/panic", nil)
	rec := httptest.NewRecorder()

	func() {
		defer func() {
			if rv := recover(); rv == nil {
				t.Fatalf("expected middleware to re-raise the panic")
			}
		}()
		handler.ServeHTTP(rec, req)
	}()

	var record map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &record); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, buf.String())
	}

	if record["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", record["level"])
	}
	pv, ok := record["panic_value"]
	if !ok {
		t.Fatalf("log record missing panic_value field; got: %v", record)
	}
	if pv != "boom" {
		t.Errorf("panic_value = %v, want \"boom\"", pv)
	}
}

// TestLogPanic_DirectUse verifies the exported helper that CARD-009 reuses logs
// at ERROR with panic_value.
func TestLogPanic_DirectUse(t *testing.T) {
	buf := &syncBuffer{}
	logger := New(buf, "json", "info")

	LogPanic(context.Background(), logger, "kaboom")

	var record map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &record); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if record["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", record["level"])
	}
	if record["panic_value"] != "kaboom" {
		t.Errorf("panic_value = %v, want kaboom", record["panic_value"])
	}
}
