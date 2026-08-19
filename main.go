package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-relay/internal/cli"
	"agent-relay/internal/config"
	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
	"agent-relay/internal/logging"
	"agent-relay/internal/relay"

	"github.com/mark3labs/mcp-go/server"
)

var Version = "dev"

// settingSeconds parses a settings value as a positive number of seconds, or
// returns 0 (→ the detector falls back to its default for that threshold).
func settingSeconds(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version":
			fmt.Printf("agent-relay %s\n", Version)
			return
		case "--help", "-h":
			cli.Run([]string{"help"})
			return
		case "serve":
			startServer()
			return
		case "mcp":
			startStdioMCP()
			return
		case "hooks":
			cli.RunHooks(hookScripts, os.Args[2:])
			return
		case "skill":
			cli.RunSkill(os.Args[2:])
			return
		case "init", "update", "status", "agents", "inbox", "send", "thread", "stats", "conversations", "memories":
			cli.Run(os.Args[1:])
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
			cli.Run([]string{"help"})
			os.Exit(1)
		}
	}

	// No args → start server (backward compat).
	startServer()
}

// startStdioMCP runs the relay's MCP server over stdio (stdin/stdout) — the
// transport MCP clients launch directly (e.g. via an .mcpb bundle published to the
// MCP registry). Exposes the same tools as the HTTP server; logs go to stderr so
// they never corrupt the JSON-RPC stream on stdout. Blocks until the client closes.
func startStdioMCP() {
	logging.Setup()

	cfg := config.Load()
	cfg.Version = Version

	database, err := db.New()
	if err != nil {
		slog.Error("failed to init database", "err", err)
		os.Exit(1)
	}
	defer func() { _ = database.Close() }()

	ingester, err := ingest.New(ingest.Config{
		SessionProvider: func() map[string]bool {
			return database.GetKnownSessionIDs()
		},
		AgentResolver: func(sessionID string) (string, string, bool) {
			project, name, found, _ := database.GetAgentBySessionID(sessionID)
			return project, name, found
		},
		Thresholds: func() ingest.Thresholds {
			return ingest.Thresholds{
				Waiting: settingSeconds(database.GetSetting("activity_waiting_seconds")),
				Idle:    settingSeconds(database.GetSetting("activity_idle_seconds")),
				Exit:    settingSeconds(database.GetSetting("activity_exit_seconds")),
			}
		},
	})
	if err != nil {
		slog.Error("failed to init ingester", "err", err)
		os.Exit(1)
	}
	defer ingester.Stop()

	r := relay.New(database, ingester, cfg)
	r.Version = Version

	if err := server.ServeStdio(r.MCPServer); err != nil {
		slog.Error("stdio MCP server error", "err", err)
		os.Exit(1)
	}
}

func startServer() {
	logging.Setup()

	cfg := config.Load()
	cfg.Version = Version

	// Single-writer guard: refuse to start if another relay already serves this
	// DB. Two relays on one SQLite file corrupt it (this is what wiped agents +
	// teams when a stray launchd relay came up on the same database).
	if dbPath, perr := db.DBPath(); perr == nil {
		if release, lerr := acquireServeLock(dbPath + ".lock"); lerr != nil {
			slog.Error("another agent-relay is already serving this DB — refusing to start a second writer (it corrupts the DB); stop the other instance first",
				"db", dbPath, "err", lerr)
			os.Exit(1)
		} else {
			defer release()
		}
	}

	database, err := db.New()
	if err != nil {
		slog.Error("failed to init database", "err", err)
		os.Exit(1)
	}
	defer func() { _ = database.Close() }()

	ingester, err := ingest.New(ingest.Config{
		SessionProvider: func() map[string]bool {
			return database.GetKnownSessionIDs()
		},
		AgentResolver: func(sessionID string) (string, string, bool) {
			project, name, found, _ := database.GetAgentBySessionID(sessionID)
			return project, name, found
		},
		Thresholds: func() ingest.Thresholds {
			return ingest.Thresholds{
				Waiting: settingSeconds(database.GetSetting("activity_waiting_seconds")),
				Idle:    settingSeconds(database.GetSetting("activity_idle_seconds")),
				Exit:    settingSeconds(database.GetSetting("activity_exit_seconds")),
			}
		},
	})
	if err != nil {
		slog.Error("failed to init ingester", "err", err)
		os.Exit(1)
	}
	defer ingester.Stop()

	r := relay.New(database, ingester, cfg)
	r.Version = Version

	// Bind loopback-only by default. RELAY_BIND overrides the host (e.g.
	// "0.0.0.0" to expose on the LAN); PORT overrides the port.
	host := os.Getenv("RELAY_BIND")
	if host == "" {
		host = "127.0.0.1"
	}
	port := "8090"
	if v := os.Getenv("PORT"); v != "" {
		port = v
	}
	addr := net.JoinHostPort(host, port)

	// Refuse to expose a non-loopback bind without authentication — otherwise the
	// entire API/MCP surface is open to everything on the network.
	if !isLoopbackHost(host) && cfg.APIKey == "" {
		slog.Error("refusing to bind without auth: set RELAY_API_KEY to expose on a non-loopback address, or unset RELAY_BIND to bind 127.0.0.1",
			"addr", addr)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start background goroutines.
	cleanupDone := make(chan struct{})
	relay.StartCleanup(database, cleanupDone)
	relay.StartACKChecker(database, r.Registry, cleanupDone)

	// Start notifications subsystem (rules evaluator + digest scheduler)
	if r.Notifier != nil {
		r.Notifier.Start(cleanupDone)
	}

	// Start the cron scheduler (recurring typed tickets). Inert until the
	// "cron_schedules" setting is populated.
	r.StartCronScheduler(cleanupDone)

	// Start the task maintenance sweeper (G3/G4): converge tasks whose PR merged
	// but never transitioned, and requeue tasks held by a dead agent past lease
	// expiry — self-healing, no external poller required.
	r.StartTaskMaintenanceSweeper(cleanupDone)

	// Wire the Linear connector from effective config (env or settings table).
	// Inert when unconfigured; hot-reloaded on settings changes without restart.
	r.ReconfigureLinear()

	// Load federation peers from effective config (env RELAY_FEDERATION_PEERS wins,
	// else the settings table). Inert when no peers; hot-reloaded via /api/settings.
	r.ReconfigureFederation()

	// Log ingested events (phase 1: log only, phase 2: TouchAgent + WS broadcast)
	go func() {
		for evt := range ingester.Events {
			log.Printf("[ingest] %s session=%s tool=%s activity=%s", evt.Type, evt.SessionID, evt.Tool, evt.Activity)
		}
	}()

	// Startup log with security status.
	authStatus := "disabled"
	if cfg.APIKey != "" {
		authStatus = "enabled"
	}
	corsStatus := "same-origin"
	if len(cfg.CORSOrigins) > 0 {
		corsStatus = fmt.Sprintf("%v", cfg.CORSOrigins)
	}
	rateLimitStatus := "disabled"
	if cfg.RateLimit > 0 {
		rateLimitStatus = fmt.Sprintf("%d/min", cfg.RateLimit)
	}
	bodyStatus := "unlimited"
	if cfg.MaxBody > 0 {
		bodyStatus = fmt.Sprintf("%dKB", cfg.MaxBody/1024)
	}
	log.Printf("agent-relay starting on %s", addr)
	log.Printf("  auth: %s | cors: %s | max body: %s | rate limit: %s | identity: enforced (no anonymous/default)",
		authStatus, corsStatus, bodyStatus, rateLimitStatus)

	// serveErr surfaces a bind/listen failure (e.g. EADDRINUSE when a stale
	// relay still holds the port after sleep/wake) so we exit non-zero instead
	// of hanging idle while an old process keeps serving the UI.
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("listening on %s (UI: http://localhost:%s)", addr, port)
		if err := r.ListenAndServe(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		if isAddrInUse(err) {
			slog.Error("cannot bind: address already in use — another agent-relay is still running; stop it first: lsof -ti tcp:<port> | xargs kill -9",
				"addr", addr, "port", port)
			os.Exit(1)
		}
		slog.Error("server failed", "err", err)
		os.Exit(1)
	case <-ctx.Done():
	}
	close(cleanupDone)
	log.Println("shutting down...")
	// Bound the shutdown: Shutdown cancels SSE streams so they unblock, but cap
	// the wait so a stuck handler can't hang the process forever.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := r.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}

// isAddrInUse reports whether err is an EADDRINUSE listen failure — the case
// where a stale relay (often surviving a sleep/wake cycle) still holds the port.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// isLoopbackHost reports whether a bind host is loopback-only (safe to serve
// without auth). Anything else exposes the relay beyond the local machine.
func isLoopbackHost(h string) bool {
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
