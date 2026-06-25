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

// Result is the outcome of evaluating every registered checker. It is the
// transport-agnostic readiness verdict: the HTTP boundary (the api-generated
// /readyz handler in the server package, or Handler.Ready below) maps it to a
// status code and a response body. Dependencies maps each dependency name to
// "ok" or to the error description (e.g. "redis:down" -> reason).
type Result struct {
	// Healthy is true only when every registered checker reported nil.
	Healthy bool
	// Dependencies is the per-dependency status map: "ok" on success, the
	// error string on failure. With no checkers registered it is empty and
	// Healthy is true.
	Dependencies map[string]string
}

// checkResult is the outcome of a single Checker.Check call, carried through
// a channel so Evaluate can collect results from concurrent goroutines.
type checkResult struct {
	name string
	err  error
}

// Evaluate runs every registered checker CONCURRENTLY, each under an
// individual per-check deadline derived from h.timeout (so a slow first check
// cannot starve the rest). It returns the aggregated Result (FR-009).
//
// All goroutines are bound by ctx: they exit no later than when the
// context-with-timeout derived here is cancelled, so there is no goroutine
// leak even if a checker blocks.
func (h *Handler) Evaluate(ctx context.Context) Result {
	h.mu.RLock()
	checkers := make([]Checker, len(h.checkers))
	copy(checkers, h.checkers)
	h.mu.RUnlock()

	if len(checkers) == 0 {
		return Result{Healthy: true, Dependencies: map[string]string{}}
	}

	// Per-check timeout: each goroutine gets its own bounded deadline so one
	// slow checker cannot consume the entire budget at the expense of others.
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	results := make(chan checkResult, len(checkers))
	for _, c := range checkers {
		c := c // capture for goroutine
		go func() {
			results <- checkResult{name: c.Name(), err: c.Check(ctx)}
		}()
	}

	deps := make(map[string]string, len(checkers))
	healthy := true
	for range checkers {
		r := <-results
		if r.err != nil {
			healthy = false
			deps[r.name] = r.err.Error()
			h.logger.LogAttrs(ctx, slog.LevelWarn, "readiness check failed",
				slog.String("dependency", r.name),
				slog.String("error", r.err.Error()),
			)
		} else {
			deps[r.name] = "ok"
		}
	}

	return Result{Healthy: healthy, Dependencies: deps}
}

// Ready handles GET /readyz: 200 when every registered checker is healthy, 503
// when at least one reports an error (FR-009). With no checkers registered the
// readiness framework still answers 200 (the dependency wiring lands in
// CARD-003).
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	res := h.Evaluate(r.Context())

	status := http.StatusOK
	overall := "ok"
	if !res.Healthy {
		status = http.StatusServiceUnavailable
		overall = "unavailable"
	}
	writeJSON(w, status, map[string]any{"status": overall, "dependencies": res.Dependencies})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
