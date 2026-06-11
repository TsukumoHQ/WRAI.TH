package relay

import (
	"context"
	"io/fs"
	"log"
	"net/http"
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
		server.WithLogging(),
		server.WithRecovery(),
		server.WithToolFilter(toolsModeFilter),
	)

	events := NewEventBus()
	registry := NewSessionRegistry(mcpSrv)
	handlers := NewHandlers(database, registry, ingester, events)

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
	handlers.SetNotifier(notifier)

	// The Linear connector is wired after construction via ReconfigureLinear()
	// (env or settings driven); until then every call site sees Noop.
	handlers.SetConnector(connector.Noop{})

	serverTools = append(serverTools,
		server.ServerTool{Tool: discoverToolsTool(), Handler: handlers.HandleDiscoverTools},
		server.ServerTool{Tool: callToolTool(), Handler: handlers.HandleCallTool},
	)
	mcpSrv.AddTools(serverTools...)

	httpSrv := server.NewStreamableHTTPServer(
		mcpSrv,
		server.WithHTTPContextFunc(HTTPContextFunc),
		server.WithEndpointPath("/mcp"),
		server.WithStateLess(true),
	)

	return &Relay{
		MCPServer: mcpSrv,
		HTTP:      httpSrv,
		DB:        database,
		Registry:  registry,
		Ingester:  ingester,
		Events:    events,
		Handlers:  handlers,
		Notifier:  notifier,
		taskConn:  connector.Noop{},
		Config:    cfg,
		StartedAt: time.Now().UTC(),
	}
}

// ListenAndServe starts a composite HTTP server that serves:
//   - /mcp     → MCP Streamable HTTP handler
//   - /api/*   → REST API for the web UI
//   - /*       → Embedded static files (web UI)
func (r *Relay) ListenAndServe(addr string) error {
	mux := http.NewServeMux()

	// MCP handler
	mux.Handle("/mcp", r.HTTP)

	// REST API
	mux.HandleFunc("/api/", r.ServeAPI)

	// Embedded static files
	staticFS, err := fs.Sub(web.StaticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create sub FS: %v", err)
	}
	mux.Handle("/", http.FileServerFS(staticFS))

	handler := r.buildMiddlewareChain(mux)
	r.httpServer = &http.Server{Addr: addr, Handler: handler}
	return r.httpServer.ListenAndServe()
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

// Shutdown gracefully stops the HTTP server.
func (r *Relay) Shutdown(ctx context.Context) error {
	if r.httpServer != nil {
		return r.httpServer.Shutdown(ctx)
	}
	return nil
}
