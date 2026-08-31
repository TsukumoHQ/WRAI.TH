package relay

import (
	"context"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"agent-relay/internal/config"
	"agent-relay/internal/connector"
	linearconn "agent-relay/internal/connector/linear"
	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
	"agent-relay/internal/web"

	"github.com/mark3labs/mcp-go/server"
)

// Relay is the main struct that wires together the MCP server, DB, and notifications.
type Relay struct {
	MCPServer *server.MCPServer
	HTTP      *server.StreamableHTTPServer
	DB        *db.DB
	Registry  *SessionRegistry
	Ingester  *ingest.Ingester
	Events    *EventBus
	Handlers  *Handlers
	Notifier  *Notifier
	// Federation forwards direct messages between this relay and trusted peers.
	// Shared with Handlers; disabled (no peers) unless RELAY_FEDERATION_PEERS set.
	Federation *Federation
	// Linear connector runtime — swapped at runtime by ReconfigureLinear()
	// (settings-driven, no restart). Read through LinearConnector()/TaskConn().
	linearMu   sync.RWMutex
	linearConn *linearconn.Connector   // nil when inactive
	taskConn   connector.TaskConnector // Noop when inactive
	linearStop chan struct{}           // closes the current reconcile loop
	Config     config.Config
	// Version is the build tag, injected from main.Version.
	// Defaults to "dev" when built without ldflags.
	Version    string
	httpServer *http.Server
	StartedAt  time.Time
	// shutdownCtx is cancelled by Shutdown so long-lived handlers (SSE streams)
	// unblock and return — otherwise http.Server.Shutdown waits forever on them
	// (their only other exit is the client closing the request).
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

// New creates a fully wired Relay with all tools registered.
// Caller should set r.Version after construction if known (injected from main.Version).
func New(database *db.DB, ingester *ingest.Ingester, cfg config.Config) *Relay {
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	mcpSrv := server.NewMCPServer(
		"wrai.th",
		version,
		server.WithToolCapabilities(false),
		// CCAR-G2: advertise read-only resources (no subscribe, no listChanged —
		// the catalogs are read on demand, not pushed).
		server.WithResourceCapabilities(false, false),
		server.WithLogging(),
		server.WithRecovery(),
		server.WithToolFilter(toolsModeFilter),
	)

	events := NewEventBus()
	registry := NewSessionRegistry(mcpSrv)
	handlers := NewHandlers(database, registry, ingester, events)

	// Federation registry — shared between the send path (Handlers) and the
	// inbound REST route (Relay). Empty peer list => disabled, no behavior change.
	federation := NewFederation(cfg.FederationPeers)
	handlers.federation = federation

	// Register every tool from the registry (single source of truth in
	// toolset.go), plus the discovery pair used by ?tools=discovery
	// connections. toolsModeFilter decides which side a session sees.
	regTools := handlers.toolRegistry()
	serverTools := make([]server.ServerTool, 0, len(regTools)+2)
	for _, rt := range regTools {
		serverTools = append(serverTools, rt.ServerTool)
	}
	// Initialize notifications subsystem (rules evaluator + digest scheduler).
	// Seeds default rules on first run.
	notifier := NewNotifier(database, registry, events)
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	handlers.SetNotifier(notifier)

	// The Linear connector is wired after construction via ReconfigureLinear()
	// (env or settings driven); until then every call site sees Noop.
	handlers.SetConnector(connector.Noop{})

	serverTools = append(serverTools,
		server.ServerTool{Tool: discoverToolsTool(), Handler: handlers.HandleDiscoverTools},
		server.ServerTool{Tool: callToolTool(), Handler: handlers.HandleCallTool},
	)
	mcpSrv.AddTools(serverTools...)

	// CCAR-G2: read-only content catalogs (task board, roster, boards, memory/
	// decisions index) as MCP resources so agents see available data at connect
	// time without an exploratory tool call.
	handlers.RegisterResources(mcpSrv)

	httpSrv := server.NewStreamableHTTPServer(
		mcpSrv,
		server.WithHTTPContextFunc(HTTPContextFunc),
		server.WithEndpointPath("/mcp"),
		server.WithStateLess(true),
	)

	return &Relay{
		MCPServer:      mcpSrv,
		HTTP:           httpSrv,
		DB:             database,
		Registry:       registry,
		Ingester:       ingester,
		Events:         events,
		Handlers:       handlers,
		Notifier:       notifier,
		Federation:     federation,
		taskConn:       connector.Noop{},
		Config:         cfg,
		StartedAt:      time.Now().UTC(),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}
}

// buildHandler assembles the composite HTTP handler that serves:
//   - /mcp     → MCP Streamable HTTP handler
//   - /api/*   → REST API for the web UI
//   - /*       → Embedded static files (web UI)
func (r *Relay) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// MCP handler
	mux.Handle("/mcp", r.HTTP)

	// REST API
	mux.HandleFunc("/api/", r.ServeAPI)

	// Static UI. A RELAY_UI_DIR override serves the UI from disk so UI changes
	// deploy with a file copy — no rebuild, no relay restart (and thus no MCP-pipe
	// drop). Falls back to the embedded build when unset/invalid.
	var uiHandler http.Handler
	if dir := os.Getenv("RELAY_UI_DIR"); dir != "" {
		if st, serr := os.Stat(dir); serr == nil && st.IsDir() {
			log.Printf("serving UI from disk override RELAY_UI_DIR=%s", dir)
			uiHandler = http.FileServer(http.Dir(dir))
		} else {
			log.Printf("RELAY_UI_DIR=%s is not a directory — falling back to embedded UI", dir)
		}
	}
	if uiHandler == nil {
		staticFS, err := fs.Sub(web.StaticFiles, "static")
		if err != nil {
			log.Fatalf("failed to create sub FS: %v", err)
		}
		uiHandler = http.FileServerFS(staticFS)
	}
	mux.Handle("/", uiHandler)

	return r.buildMiddlewareChain(mux)
}

// ListenAndServe binds addr and serves the composite handler. Kept for
// callers that don't need to bind the listener ahead of heavy init — prefer
// Serve with a listener opened earlier so the port is reachable before
// migrations/backups run.
func (r *Relay) ListenAndServe(addr string) error {
	r.httpServer = &http.Server{Addr: addr, Handler: r.buildHandler()}
	return r.httpServer.ListenAndServe()
}

// Serve runs the composite handler on an already-open listener. Callers bind
// the listener (net.Listen) as early as possible — before database init and
// migrations — so the port is reachable and a "listening" log line appears
// immediately, instead of only after however long that heavy work takes.
func (r *Relay) Serve(ln net.Listener) error {
	r.httpServer = &http.Server{Addr: ln.Addr().String(), Handler: r.buildHandler()}
	return r.httpServer.Serve(ln)
}

// buildMiddlewareChain wraps the mux with security middleware.
// Order: CORS (outermost) → RateLimit → BodyLimit → Auth → handler.
func (r *Relay) buildMiddlewareChain(handler http.Handler) http.Handler {
	handler = authMiddleware(r.Config.APIKey, handler)
	handler = bodySizeLimitMiddleware(r.Config.MaxBody, handler)
	handler = rateLimitMiddleware(r.Config.RateLimit, handler)
	handler = corsMiddleware(r.Config.CORSOrigins, handler)
	return handler
}

// Shutdown gracefully stops the HTTP server. It first cancels shutdownCtx so
// long-lived SSE handlers unblock and return, then waits for in-flight handlers
// via http.Server.Shutdown (bounded by the caller's ctx).
func (r *Relay) Shutdown(ctx context.Context) error {
	if r.shutdownCancel != nil {
		r.shutdownCancel()
	}
	if r.httpServer != nil {
		return r.httpServer.Shutdown(ctx)
	}
	return nil
}
