package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"agent-relay/internal/cli"
	"agent-relay/internal/config"
	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
	"agent-relay/internal/relay"
)

var Version = "dev"

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

func startServer() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg := config.Load()
	cfg.Version = Version

	database, err := db.New()
	if err != nil {
		log.Fatalf("failed to init database: %v", err)
	}
	defer func() { _ = database.Close() }()

	ingester, err := ingest.New(ingest.Config{
		SessionProvider: func() map[string]bool {
			return database.GetKnownSessionIDs()
		},
	})
	if err != nil {
		log.Fatalf("failed to init ingester: %v", err)
	}
	defer ingester.Stop()

	r := relay.New(database, ingester, cfg)
	r.Version = Version

	addr := ":8090"
	if v := os.Getenv("PORT"); v != "" {
		addr = ":" + v
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

	// Wire the Linear connector from effective config (env or settings table).
	// Inert when unconfigured; hot-reloaded on settings changes without restart.
	r.ReconfigureLinear()

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
	log.Printf("agent-relay starting on %s", addr)
	log.Printf("  auth: %s | cors: %s | max body: %dB | rate limit: %s",
		authStatus, corsStatus, cfg.MaxBody, rateLimitStatus)

	go func() {
		log.Printf("listening on %s (UI: http://localhost%s)", addr, addr)
		if err := r.ListenAndServe(addr); err != nil {
			log.Printf("server stopped: %v", err)
		}
	}()

	<-ctx.Done()
	close(cleanupDone)
	log.Println("shutting down...")
	if err := r.Shutdown(context.Background()); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
