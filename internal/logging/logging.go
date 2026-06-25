// Package logging provides the structured slog logger (COMP-015) and the
// per-request HTTP middleware required by FR-016. The logger is constructed and
// injected via DI — there are no global singletons (ADR-0008).
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New constructs a *slog.Logger writing to w. format is "json" (production) or
// "text" (local dev); level is one of debug/info/warn/error (defaulting to
// info for any unrecognised value). The returned logger is safe for concurrent
// use and is the single instance threaded through the service via DI.
func New(w io.Writer, format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
