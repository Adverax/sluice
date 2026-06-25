// Package lifecycle implements COMP-006, the Lifecycle Manager: it owns the
// inbound *http.Server, runs it, and performs graceful shutdown on
// SIGINT/SIGTERM, draining in-flight requests before the process exits
// (FR-012 / NFR-005). The logger and server are injected — no globals
// (ADR-0008).
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Manager coordinates serving and graceful shutdown of a single http.Server.
type Manager struct {
	server          *http.Server
	logger          *slog.Logger
	shutdownTimeout time.Duration

	// inFlight counts requests currently being served. It is incremented by the
	// CountingMiddleware and sampled at shutdown to log how many requests were
	// drained.
	inFlight int64

	// onShutdown holds post-drain hooks run AFTER the HTTP server has drained
	// in-flight requests (or the drain timed out), in registration order, each
	// bounded by the same shutdown deadline. The metering worker's Close is
	// registered here so remaining buffered usage events are flushed before exit
	// (AC-032 / FR-012). Hooks run on both the clean and forced shutdown paths so
	// metering is always flushed.
	onShutdown []func(context.Context) error
}

// New constructs a lifecycle Manager. shutdownTimeout bounds the drain; a value
// <= 0 falls back to 30s.
func New(server *http.Server, logger *slog.Logger, shutdownTimeout time.Duration) *Manager {
	if shutdownTimeout <= 0 {
		shutdownTimeout = 30 * time.Second
	}
	return &Manager{
		server:          server,
		logger:          logger,
		shutdownTimeout: shutdownTimeout,
	}
}

// OnShutdown registers a hook to run during graceful shutdown AFTER the HTTP
// server drain completes (or the drain deadline elapses). Hooks run in
// registration order on both the clean and forced paths, so resources like the
// metering worker (AC-032) are always given a chance to flush before exit. A nil
// hook is ignored. Register hooks before Run.
func (m *Manager) OnShutdown(hook func(context.Context) error) {
	if hook != nil {
		m.onShutdown = append(m.onShutdown, hook)
	}
}

// CountingMiddleware tracks in-flight requests so the drain count can be logged
// on shutdown. It must wrap the handlers whose requests should be counted as
// "in-flight" for FR-012 / NFR-005.
func (m *Manager) CountingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&m.inFlight, 1)
		defer atomic.AddInt64(&m.inFlight, -1)
		next.ServeHTTP(w, r)
	})
}

// InFlight returns the current number of in-flight requests.
func (m *Manager) InFlight() int64 { return atomic.LoadInt64(&m.inFlight) }

// Run starts the server and blocks until ctx is cancelled (e.g. by
// signal.NotifyContext on SIGINT/SIGTERM), then gracefully shuts the server
// down, draining in-flight requests within shutdownTimeout. It returns nil on a
// clean drain. The caller maps a nil error to exit code 0.
func (m *Manager) Run(ctx context.Context) error {
	serveErr := make(chan error, 1)
	go func() {
		m.logger.LogAttrs(ctx, slog.LevelInfo, "server listening",
			slog.String("addr", m.server.Addr))
		if err := m.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("server failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		return m.shutdown()
	}
}

// shutdown performs the graceful drain bounded by shutdownTimeout. The number of
// in-flight requests is sampled at the start of the drain so the "drained N
// requests" line reflects what was actually drained.
//
// Forced shutdown (AC-051): if the in-flight requests do not drain within
// shutdownTimeout, server.Shutdown returns context.DeadlineExceeded. We do NOT
// treat this as a hard failure — the process must still exit cleanly (forced).
// We log "forced shutdown: N requests unfinished" with the count of still
// in-flight requests, then proceed to the post-drain hooks. A non-deadline
// Shutdown error is the only path mapped to a returned error.
//
// On both the clean and forced paths the post-drain hooks (e.g. the metering
// worker's Close — AC-032) run within the SAME deadline so buffered usage events
// are flushed before exit.
func (m *Manager) shutdown() error {
	draining := atomic.LoadInt64(&m.inFlight)
	m.logger.LogAttrs(context.Background(), slog.LevelInfo, "shutdown signal received, draining",
		slog.Int64("in_flight", draining))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
	defer cancel()

	var shutdownErr error
	if err := m.server.Shutdown(shutdownCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// Forced shutdown: in-flight requests did not drain in time (AC-051).
			// Log the count of unfinished requests; do not fail the process exit.
			unfinished := atomic.LoadInt64(&m.inFlight)
			m.logger.LogAttrs(context.Background(), slog.LevelWarn,
				fmt.Sprintf("forced shutdown: %d requests unfinished", unfinished),
				slog.Int64("unfinished", unfinished))
		} else {
			m.logger.LogAttrs(context.Background(), slog.LevelError, "graceful shutdown failed",
				slog.String("error", err.Error()),
				slog.Int64("in_flight", atomic.LoadInt64(&m.inFlight)))
			shutdownErr = fmt.Errorf("graceful shutdown: %w", err)
		}
	} else {
		m.logger.LogAttrs(context.Background(), slog.LevelInfo,
			fmt.Sprintf("drained %d requests", draining),
			slog.Int64("drained", draining))
	}

	// Post-drain hooks (AC-032): flush the metering buffer etc. before exit. Run
	// them even on the forced path so usage events are not lost. They share the
	// shutdown deadline. The first hook error is surfaced if no earlier error.
	for _, hook := range m.onShutdown {
		if err := hook(shutdownCtx); err != nil {
			m.logger.LogAttrs(context.Background(), slog.LevelError, "shutdown hook failed",
				slog.String("error", err.Error()))
			if shutdownErr == nil {
				shutdownErr = fmt.Errorf("shutdown hook: %w", err)
			}
		}
	}

	return shutdownErr
}
