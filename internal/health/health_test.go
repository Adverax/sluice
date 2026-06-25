package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestHandler() *Handler {
	return New(slog.New(slog.NewJSONHandler(io.Discard, nil)), 0)
}

func TestLive_AlwaysOK(t *testing.T) {
	h := newTestHandler()
	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("Live status = %d, want 200", rec.Code)
	}
}

func TestReady_NoCheckers_OK(t *testing.T) {
	h := newTestHandler()
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("Ready (no checkers) status = %d, want 200", rec.Code)
	}
}

func TestReady_AllHealthy_OK(t *testing.T) {
	h := newTestHandler()
	h.Register(
		CheckerFunc{CheckerName: "redis", CheckFunc: func(context.Context) error { return nil }},
		CheckerFunc{CheckerName: "postgres", CheckFunc: func(context.Context) error { return nil }},
	)
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("Ready status = %d, want 200", rec.Code)
	}
}

// TestReady_DependencyUnhealthy_503 delivers the readiness-framework AC for
// FR-009: /readyz returns 503 when at least one dependency reports unhealthy.
func TestReady_DependencyUnhealthy_503(t *testing.T) {
	h := newTestHandler()
	h.Register(
		CheckerFunc{CheckerName: "redis", CheckFunc: func(context.Context) error { return nil }},
		CheckerFunc{CheckerName: "postgres", CheckFunc: func(context.Context) error {
			return errors.New("connection refused")
		}},
	)
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Ready status = %d, want 503", rec.Code)
	}
}
