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

// shutdown performs the graceful drain. The number of in-flight requests is
// sampled at the start of the drain so the "drained N requests" line reflects
// what was actually drained.
func (m *Manager) shutdown() error {
	draining := atomic.LoadInt64(&m.inFlight)
	m.logger.LogAttrs(context.Background(), slog.LevelInfo, "shutdown signal received, draining",
		slog.Int64("in_flight", draining))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
	defer cancel()

	if err := m.server.Shutdown(shutdownCtx); err != nil {
		m.logger.LogAttrs(context.Background(), slog.LevelError, "graceful shutdown failed",
			slog.String("error", err.Error()),
			slog.Int64("in_flight", atomic.LoadInt64(&m.inFlight)))
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	m.logger.LogAttrs(context.Background(), slog.LevelInfo,
		fmt.Sprintf("drained %d requests", draining),
		slog.Int64("drained", draining))
	return nil
}
