// Package logging configures the process-wide structured logger (slog) from the
// environment and bridges the stdlib log package onto it, so existing log.Printf
// call sites keep working — same handler, same format, filtered by the same
// level — without being rewritten.
package logging

import (
	"context"
	"log"
	"log/slog"
	"os"
	"strings"
)

// Setup installs slog as the default logger and points the stdlib log package at
// it. Call once, as early as possible in each entrypoint.
//
//	RELAY_LOG_LEVEL   debug | info | warn | error   (default info)
//	RELAY_LOG_FORMAT  text | json                   (default text)
//
// Output always goes to stderr: on the stdio MCP transport stdout carries the
// JSON-RPC stream and must never be polluted by a log line.
func Setup() {
	level := ParseLevel(os.Getenv("RELAY_LOG_LEVEL"))
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RELAY_LOG_FORMAT")), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))

	// Bridge the stdlib global logger onto slog. Drop its flags — slog stamps
	// time and level itself, so leaving them on would double the timestamp — and
	// route each line through slog at INFO. A plain log.Printf therefore still
	// appears, formatted like everything else and hidden when the level is raised
	// above INFO. Startup fatals that must survive a raised level use slog.Error
	// directly (see main), so nothing load-bearing is lost when INFO is filtered.
	log.SetFlags(0)
	log.SetOutput(bridgeWriter{})
}

// ParseLevel maps a RELAY_LOG_LEVEL string to a slog.Level, defaulting to INFO
// for empty or unrecognized input.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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

// bridgeWriter turns each stdlib log line into an slog record at INFO.
type bridgeWriter struct{}

func (bridgeWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	slog.Default().LogAttrs(context.Background(), slog.LevelInfo, msg)
	return len(p), nil
}
