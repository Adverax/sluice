// Command gateway is the sluice service entry point. It wires configuration,
// the structured logger, the health/readiness framework and the lifecycle
// manager together via dependency injection (ADR-0008) and boots a
// timeout-bearing http.Server with graceful shutdown (FR-012, NFR-004,
// NFR-005, FR-016). Business behaviour (proxy, rate limiting, cache, breaker,
// metering, metrics) is delivered by later cards.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/adverax/sluice/internal/config"
	"github.com/adverax/sluice/internal/health"
	"github.com/adverax/sluice/internal/lifecycle"
	"github.com/adverax/sluice/internal/logging"
)

func main() {
	if err := run(); err != nil {
		// run() already logs the detail; emit a last-resort line and exit 1.
		slog.Error("gateway exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// run performs the full DI wiring and blocks until graceful shutdown completes.
// It returns nil on a clean drain (exit code 0).
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(os.Stdout, cfg.Logging.Format, cfg.Logging.Level)
	logger.Info("starting gateway",
		slog.String("addr", cfg.Server.Addr),
		slog.String("log_format", cfg.Logging.Format),
		slog.String("log_level", cfg.Logging.Level),
	)

	// Health/readiness framework (FR-008/FR-009). Real dependency checkers
	// (Redis, Postgres) are registered in CARD-003.
	healthHandler := health.New(logger, cfg.Redis.DialTimeout)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler.Live)
	mux.HandleFunc("GET /readyz", healthHandler.Ready)

	server := &http.Server{
		Addr:         cfg.Server.Addr,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	manager := lifecycle.New(server, logger, cfg.Server.ShutdownTimeout)

	// Middleware chain (outermost first): request logging then in-flight
	// counting, both wrapping the application mux.
	handler := logging.Middleware(logger)(manager.CountingMiddleware(mux))
	server.Handler = handler

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return manager.Run(ctx)
}
