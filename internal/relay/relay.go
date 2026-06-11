package relay

import (
	"context"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"agent-relay/internal/config"
	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
	"agent-relay/internal/lock"
	"agent-relay/internal/scheduler"
	"agent-relay/internal/spawn"
	"agent-relay/internal/vault"
	"agent-relay/internal/web"
	"agent-relay/internal/workflow"

	"github.com/mark3labs/mcp-go/server"
)

// Relay is the main struct that wires together the MCP server, DB, and notifications.
type Relay struct {
	MCPServer      *server.MCPServer
	HTTP           *server.StreamableHTTPServer
	DB             *db.DB
	Registry       *SessionRegistry
	Ingester       *ingest.Ingester
	VaultWatcher   *vault.Watcher
	Events         *EventBus
	SpawnMgr       *spawn.Manager
	Scheduler      *scheduler.Scheduler
	PTYMgr         *spawn.PTYManager
	Handlers       *Handlers
	WorkflowEngine *workflow.Engine
	Notifier       *Notifier
	Config         config.Config
	httpServer     *http.Server
	StartedAt      time.Time
}

// New creates a fully wired Relay with all tools registered.
func New(database *db.DB, ingester *ingest.Ingester, vaultWatcher *vault.Watcher, cfg config.Config) *Relay {
	mcpSrv := server.NewMCPServer(
		"wrai.th",
		"0.5.0",
		server.WithToolCapabilities(false),
		server.WithLogging(),
		server.WithRecovery(),
	)

	events := NewEventBus()
	registry := NewSessionRegistry(mcpSrv)
	handlers := NewHandlers(database, registry, ingester, vaultWatcher, events)

	// Initialize spawn infrastructure
	logger := log.Default()
	slogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	executor := spawn.NewExecutor(cfg.ClaudeBinary, slogger)
	lockMgr := lock.NewManager(cfg.LocksDir, "relay-", slogger)
	queue := lock.NewPriorityQueue(slogger)
	sched := scheduler.New(slogger)

	var spawnMgr *spawn.Manager
	var ptyMgr *spawn.PTYManager
	if executor.IsClaudeAvailable() {
		spawnMgr = spawn.NewManager(database, executor, lockMgr, queue, sched, cfg.MaxPoolSize, slogger)
		ptyMgr = spawn.NewPTYManager(executor)
		handlers.SetSpawnManager(spawnMgr)
		logger.Println("spawn: claude binary found, spawn/schedule/terminal enabled")
	} else {
		logger.Println("spawn: claude binary not found, spawn/schedule disabled")
	}

	// Initialize workflow engine
	wfEngine := workflow.NewEngine(database, spawnMgr)
	// Wire message and task functions so workflows can send messages and dispatch tasks
	wfEngine.SetMessageFunc(func(project, from, to, msgType, subject, content string) error {
		_, err := database.InsertMessage(project, from, to, msgType, subject, content, "{}", "P2", 0, nil, nil)
		return err
	})
	wfEngine.SetTaskFunc(func(project, profile, title, desc string) (string, error) {
		task, err := database.DispatchTask(project, profile, "workflow-engine", title, desc, "P2", nil, nil, nil)
		if err != nil {
			return "", err
		}
		return task.ID, nil
	})

	handlers.SetWorkflowEngine(wfEngine)

	// Initialize notifications subsystem (rules evaluator + digest scheduler).
	// Seeds default rules on first run.
	notifier := NewNotifier(database, registry, events)
	handlers.SetNotifier(notifier)

	// Register all tools
	mcpSrv.AddTools(
		server.ServerTool{Tool: whoamiTool(), Handler: handlers.HandleWhoami},
		server.ServerTool{Tool: registerAgentTool(), Handler: handlers.HandleRegisterAgent},
		server.ServerTool{Tool: sendMessageTool(), Handler: handlers.HandleSendMessage},
		server.ServerTool{Tool: getInboxTool(), Handler: handlers.HandleGetInbox},
		server.ServerTool{Tool: ackDeliveryTool(), Handler: handlers.HandleAckDelivery},
		server.ServerTool{Tool: getThreadTool(), Handler: handlers.HandleGetThread},
		server.ServerTool{Tool: listAgentsTool(), Handler: handlers.HandleListAgents},
		server.ServerTool{Tool: markReadTool(), Handler: handlers.HandleMarkRead},
		server.ServerTool{Tool: createConversationTool(), Handler: handlers.HandleCreateConversation},
		server.ServerTool{Tool: listConversationsTool(), Handler: handlers.HandleListConversations},
		server.ServerTool{Tool: getConversationMessagesTool(), Handler: handlers.HandleGetConversationMessages},
		server.ServerTool{Tool: inviteToConversationTool(), Handler: handlers.HandleInviteToConversation},
		server.ServerTool{Tool: leaveConversationTool(), Handler: handlers.HandleLeaveConversation},
		server.ServerTool{Tool: archiveConversationTool(), Handler: handlers.HandleArchiveConversation},
		// Memory tools
		server.ServerTool{Tool: setMemoryTool(), Handler: handlers.HandleSetMemory},
		server.ServerTool{Tool: getMemoryTool(), Handler: handlers.HandleGetMemory},
		server.ServerTool{Tool: searchMemoryTool(), Handler: handlers.HandleSearchMemory},
		server.ServerTool{Tool: listMemoriesTool(), Handler: handlers.HandleListMemories},
		server.ServerTool{Tool: deleteMemoryTool(), Handler: handlers.HandleDeleteMemory},
		server.ServerTool{Tool: resolveConflictTool(), Handler: handlers.HandleResolveConflict},
		// Profile tools
		server.ServerTool{Tool: registerProfileTool(), Handler: handlers.HandleRegisterProfile},
		server.ServerTool{Tool: getProfileTool(), Handler: handlers.HandleGetProfile},
		server.ServerTool{Tool: listProfilesTool(), Handler: handlers.HandleListProfiles},
		server.ServerTool{Tool: findProfilesTool(), Handler: handlers.HandleFindProfiles},
		// Task tools
		server.ServerTool{Tool: dispatchTaskTool(), Handler: handlers.HandleDispatchTask},
		server.ServerTool{Tool: claimTaskTool(), Handler: handlers.HandleClaimTask},
		server.ServerTool{Tool: startTaskTool(), Handler: handlers.HandleStartTask},
		server.ServerTool{Tool: completeTaskTool(), Handler: handlers.HandleCompleteTask},
		server.ServerTool{Tool: blockTaskTool(), Handler: handlers.HandleBlockTask},
		server.ServerTool{Tool: cancelTaskTool(), Handler: handlers.HandleCancelTask},
		server.ServerTool{Tool: getTaskTool(), Handler: handlers.HandleGetTask},
		server.ServerTool{Tool: listTasksTool(), Handler: handlers.HandleListTasks},
		server.ServerTool{Tool: updateTaskTool(), Handler: handlers.HandleUpdateTask},
		server.ServerTool{Tool: archiveTasksTool(), Handler: handlers.HandleArchiveTasks},
		server.ServerTool{Tool: moveTaskTool(), Handler: handlers.HandleMoveTask},
		server.ServerTool{Tool: batchCompleteTasksTool(), Handler: handlers.HandleBatchCompleteTasks},
		server.ServerTool{Tool: batchDispatchTasksTool(), Handler: handlers.HandleBatchDispatchTasks},
		// Boards
		server.ServerTool{Tool: createBoardTool(), Handler: handlers.HandleCreateBoard},
		server.ServerTool{Tool: listBoardsTool(), Handler: handlers.HandleListBoards},
		server.ServerTool{Tool: archiveBoardTool(), Handler: handlers.HandleArchiveBoard},
		server.ServerTool{Tool: deleteBoardTool(), Handler: handlers.HandleDeleteBoard},
		// Goals
		server.ServerTool{Tool: createGoalTool(), Handler: handlers.HandleCreateGoal},
		server.ServerTool{Tool: listGoalsTool(), Handler: handlers.HandleListGoals},
		server.ServerTool{Tool: getGoalTool(), Handler: handlers.HandleGetGoal},
		server.ServerTool{Tool: updateGoalTool(), Handler: handlers.HandleUpdateGoal},
		server.ServerTool{Tool: getGoalCascadeTool(), Handler: handlers.HandleGetGoalCascade},
		// Vault
		server.ServerTool{Tool: registerVaultTool(), Handler: handlers.HandleRegisterVault},
		server.ServerTool{Tool: searchVaultTool(), Handler: handlers.HandleSearchVault},
		server.ServerTool{Tool: getVaultDocTool(), Handler: handlers.HandleGetVaultDoc},
		server.ServerTool{Tool: listVaultDocsTool(), Handler: handlers.HandleListVaultDocs},
		// File locks
		server.ServerTool{Tool: claimFilesTool(), Handler: handlers.HandleClaimFiles},
		server.ServerTool{Tool: releaseFilesTool(), Handler: handlers.HandleReleaseFiles},
		server.ServerTool{Tool: listLocksTool(), Handler: handlers.HandleListLocks},
		// Agent lifecycle
		server.ServerTool{Tool: deactivateAgentTool(), Handler: handlers.HandleDeactivateAgent},
		server.ServerTool{Tool: deleteAgentTool(), Handler: handlers.HandleDeleteAgent},
		server.ServerTool{Tool: sleepAgentTool(), Handler: handlers.HandleSleepAgent},
		// Project lifecycle
		server.ServerTool{Tool: createProjectTool(), Handler: handlers.HandleCreateProject},
		server.ServerTool{Tool: deleteProjectTool(), Handler: handlers.HandleDeleteProject},
		// Soul RAG
		server.ServerTool{Tool: queryContextTool(), Handler: handlers.HandleQueryContext},
		// Session context
		server.ServerTool{Tool: getSessionContextTool(), Handler: handlers.HandleGetSessionContext},
		// Teams + Orgs
		server.ServerTool{Tool: createOrgTool(), Handler: handlers.HandleCreateOrg},
		server.ServerTool{Tool: listOrgsTool(), Handler: handlers.HandleListOrgs},
		server.ServerTool{Tool: createTeamTool(), Handler: handlers.HandleCreateTeam},
		server.ServerTool{Tool: listTeamsTool(), Handler: handlers.HandleListTeams},
		server.ServerTool{Tool: addTeamMemberTool(), Handler: handlers.HandleAddTeamMember},
		server.ServerTool{Tool: removeTeamMemberTool(), Handler: handlers.HandleRemoveTeamMember},
		server.ServerTool{Tool: getTeamInboxTool(), Handler: handlers.HandleGetTeamInbox},
		server.ServerTool{Tool: addNotifyChannelTool(), Handler: handlers.HandleAddNotifyChannel},
		// Spawn (fork/exec)
		server.ServerTool{Tool: spawnTool(), Handler: handlers.HandleSpawn},
		server.ServerTool{Tool: killChildTool(), Handler: handlers.HandleKillChild},
		server.ServerTool{Tool: listChildrenTool(), Handler: handlers.HandleListChildren},
		// Schedule (crontab)
		server.ServerTool{Tool: scheduleTool(), Handler: handlers.HandleSchedule},
		server.ServerTool{Tool: unscheduleTool(), Handler: handlers.HandleUnschedule},
		server.ServerTool{Tool: listSchedulesTool(), Handler: handlers.HandleListSchedules},
		server.ServerTool{Tool: triggerCycleTool(), Handler: handlers.HandleTriggerCycle},
	)

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
		VaultWatcher:   vaultWatcher,
		Events:         events,
		SpawnMgr:       spawnMgr,
		PTYMgr:         ptyMgr,
		Scheduler:      sched,
		Handlers:       handlers,
		WorkflowEngine: wfEngine,
		Notifier:       notifier,
		Config:         cfg,
		StartedAt:      time.Now().UTC(),
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
