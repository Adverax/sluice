// Package health implements the liveness (/healthz, FR-008) and readiness
// (/readyz, FR-009) probes for COMP-015 / CTX-003. Readiness is built on a
// small Checker port so CARD-003 can register real Redis/Postgres checks
// without changing this package.
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Checker is the readiness port. An implementation reports whether a single
// dependency (e.g. Redis or Postgres) is currently usable. Implementations
// must honour ctx cancellation/deadline.
type Checker interface {
	// Name identifies the dependency in the readiness response.
	Name() string
	// Check returns nil when the dependency is healthy, or an error describing
	// why it is not.
	Check(ctx context.Context) error
}

// CheckerFunc adapts a function into a Checker.
type CheckerFunc struct {
	CheckerName string
	CheckFunc   func(ctx context.Context) error
}

// Name implements Checker.
func (c CheckerFunc) Name() string { return c.CheckerName }

// Check implements Checker.
func (c CheckerFunc) Check(ctx context.Context) error { return c.CheckFunc(ctx) }

// Handler aggregates readiness checkers and serves the probe endpoints.
// The zero value is not usable; construct it with New.
type Handler struct {
	logger  *slog.Logger
	timeout time.Duration

	mu       sync.RWMutex
	checkers []Checker
}

// New constructs a health Handler. timeout bounds each readiness check; if it
// is <= 0 a 2s default is applied. The logger is injected (ADR-0008).
func New(logger *slog.Logger, timeout time.Duration) *Handler {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Handler{logger: logger, timeout: timeout}
}

// Register adds one or more readiness checkers. Safe for concurrent use; in
// practice CARD-003 calls it at startup.
func (h *Handler) Register(checkers ...Checker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checkers = append(h.checkers, checkers...)
}

// Live handles GET /healthz: 200 OK as long as the process is alive (FR-008).
func (h *Handler) Live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready handles GET /readyz: 200 when every registered checker is healthy, 503
// when at least one reports an error (FR-009). With no checkers registered the
// readiness framework still answers 200 (the dependency wiring lands in
// CARD-003).
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	checkers := make([]Checker, len(h.checkers))
	copy(checkers, h.checkers)
	h.mu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	deps := make(map[string]string, len(checkers))
	healthy := true
	for _, c := range checkers {
		if err := c.Check(ctx); err != nil {
			healthy = false
			deps[c.Name()] = err.Error()
			h.logger.LogAttrs(ctx, slog.LevelWarn, "readiness check failed",
				slog.String("dependency", c.Name()),
				slog.String("error", err.Error()),
			)
		} else {
			deps[c.Name()] = "ok"
		}
	}

	status := http.StatusOK
	overall := "ok"
	if !healthy {
		status = http.StatusServiceUnavailable
		overall = "unavailable"
	}
	writeJSON(w, status, map[string]any{"status": overall, "dependencies": deps})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
