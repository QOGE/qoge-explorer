// Package logging configures the process-wide structured logger.
package logging

import (
	"log/slog"
	"os"
)

// New builds a structured slog.Logger writing to stderr.
//
// level accepts "debug", "info", "warn", "error" (case-insensitive; unknown
// values fall back to "info"). When jsonOutput is false, a human-readable
// text handler is used instead of JSON.
func New(level string, jsonOutput bool) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if jsonOutput {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	return slog.New(handler)
}
