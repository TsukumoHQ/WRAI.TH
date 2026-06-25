package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-relay/internal/connector"
	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
	"agent-relay/internal/models"

	"github.com/mark3labs/mcp-go/mcp"
)

type Handlers struct {
	db        *db.DB
	registry  *SessionRegistry
	ingester  *ingest.Ingester
	events    *EventBus
	tokenCh   chan db.TokenRecord
	notifier  *Notifier
	connMu    sync.RWMutex
	connector connector.TaskConnector

	// requireRegistered gates mutating tools behind a registered acting agent
	// (RELAY_REQUIRE_REGISTERED). Set from config in relay.New before dispatch.
	requireRegistered bool
}

// SetNotifier connects the notifications subsystem so handlers can emit custom
// events into the rules engine.
func (h *Handlers) SetNotifier(n *Notifier) {
	h.notifier = n
}

// SetConnector wires the task connector so the review handler can fire the one
// owned write-back (→ In Review + comment) when running in Linear mode.
func (h *Handlers) SetConnector(c connector.TaskConnector) {
	h.connMu.Lock()
	h.connector = c
	h.connMu.Unlock()
}

// getConnector returns the current task connector (hot-swappable at runtime).
func (h *Handlers) getConnector() connector.TaskConnector {
	h.connMu.RLock()
	defer h.connMu.RUnlock()
	return h.connector
}

func NewHandlers(database *db.DB, registry *SessionRegistry, ingester *ingest.Ingester, events *EventBus) *Handlers {
	h := &Handlers{db: database, registry: registry, ingester: ingester, events: events, tokenCh: make(chan db.TokenRecord, 256)}
	go h.flushTokenUsage()
	return h
}

// flushTokenUsage batches token usage records and inserts them every 5s or 50 records.
func (h *Handlers) flushTokenUsage() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	buf := make([]db.TokenRecord, 0, 64)

	flush := func() {
		if len(buf) == 0 {
			return
		}
		batch := make([]db.TokenRecord, len(buf))
		copy(batch, buf)
		buf = buf[:0]
		if err := h.db.InsertTokenUsageBatch(batch); err != nil {
			log.Printf("token usage flush error: %v", err)
		}
	}

	for {
		select {
		case r, ok := <-h.tokenCh:
			if !ok {
				flush()
				return
			}
			buf = append(buf, r)
			if len(buf) >= 50 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// RecordTokens queues a real token-usage record (from the Stop hook reading the
// transcript) onto the same batched flusher as the legacy estimate. Non-blocking:
// drops on a full buffer rather than stalling the ingest request.
func (h *Handlers) RecordTokens(rec db.TokenRecord) {
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	select {
	case h.tokenCh <- rec:
	default:
	}
}

// resultJSONTracked marshals data, records token usage, and returns the MCP result.
func (h *Handlers) resultJSONTracked(project, agent, tool string, data any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("json marshal: %v", err)), nil
	}
	select {
	case h.tokenCh <- db.TokenRecord{
		Project:   project,
		Agent:     agent,
		Tool:      tool,
		Bytes:     len(b),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}:
	default:
	}
	return mcp.NewToolResultText(string(b)), nil
}

// resultTextTracked records token usage for a plain-text result (table format).
func (h *Handlers) resultTextTracked(project, agent, tool, text string) (*mcp.CallToolResult, error) {
	select {
	case h.tokenCh <- db.TokenRecord{
		Project:   project,
		Agent:     agent,
		Tool:      tool,
		Bytes:     len(text),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}:
	default:
	}
	return mcp.NewToolResultText(text), nil
}

// renderTable renders rows as a compact (unpadded) markdown table. Keys are
// paid once in the header instead of once per element — roughly half the
// tokens of the equivalent JSON for homogeneous lists, and markdown reads
// more reliably for LLMs than raw separators.
func renderTable(header []string, rows [][]string) string {
	var sb strings.Builder
	sb.WriteByte('|')
	for _, col := range header {
		sb.WriteString(col)
		sb.WriteByte('|')
	}
	sb.WriteString("\n|")
	for range header {
		sb.WriteString("---|")
	}
	cleaner := strings.NewReplacer("|", "\\|", "\t", "  ", "\n", " ", "\r", "")
	for _, row := range rows {
		sb.WriteString("\n|")
		for _, cell := range row {
			cell = cleaner.Replace(cell)
			if cell == "" {
				cell = "-"
			}
			sb.WriteString(cell)
			sb.WriteByte('|')
		}
	}
	return sb.String()
}

func strOrDash(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// maxToolLimit caps any caller-supplied "limit" on list/get tools so a single
// call (e.g. limit=100000) can't dump the whole table into an agent's context.
// Only the upper bound is enforced — 0/negative keep their existing
// "default / unbounded" semantics in the handlers that rely on them.
const maxToolLimit = 200

func clampLimit(n int) int {
	if n > maxToolLimit {
		return maxToolLimit
	}
	return n
}

// sendCrossProject delivers a direct message to an agent in a different project.
// Both sender and recipient must be is_executive=true — this is the MVP guardrail
// for peer-to-peer cross-project DM (e.g. two CTOs coordinating between colonies).
// The message is inserted with project=targetProject (destination scope) so it
// appears in the recipient's inbox naturally. metadata.source_project and
// metadata.source_agent preserve the origin for UI rendering and reply routing.
func (h *Handlers) sendCrossProject(ctx context.Context, srcProject, from, dstProject, to, msgType, subject, content, callerMetadata string, replyTo *string, priority string, ttlSeconds int) (*mcp.CallToolResult, error) {
	// Validate sender exists and is executive
	sender, err := h.db.GetAgent(srcProject, from)
	if err != nil || sender == nil {
		return mcp.NewToolResultError(fmt.Sprintf("sender '%s' not found in project '%s'", from, srcProject)), nil
	}
	if !sender.IsExecutive {
		return mcp.NewToolResultError("cross-project messaging requires sender to be is_executive=true"), nil
	}

	// Validate target exists and is executive
	target, err := h.db.GetAgent(dstProject, to)
	if err != nil || target == nil {
		return mcp.NewToolResultError(fmt.Sprintf("target '%s' not found in project '%s'", to, dstProject)), nil
	}
	if !target.IsExecutive {
		return mcp.NewToolResultError(fmt.Sprintf("cross-project messaging requires target '%s' in project '%s' to be is_executive=true", to, dstProject)), nil
	}

	// Merge caller-provided metadata with source tracking fields
	meta := map[string]any{}
	if callerMetadata != "" && callerMetadata != "{}" {
		_ = json.Unmarshal([]byte(callerMetadata), &meta)
	}
	meta["source_project"] = srcProject
	meta["source_agent"] = from
	meta["cross_project"] = true
	metaBytes, _ := json.Marshal(meta)

	// Insert the message in the DESTINATION project scope — this is what makes
	// it visible in the recipient's get_inbox(project=dstProject) without any
	// special routing in the read path.
	msg, err := h.db.InsertMessageWithDeliveries(dstProject, from, to, msgType, subject, content, string(metaBytes), priority, ttlSeconds, replyTo, nil, []string{to})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to send cross-project message: %v", err)), nil
	}

	// Push notification if the target has an open session
	h.registry.Notify(dstProject, to, from, subject, msg.ID)

	// Visible event — scoped to SENDER project so the sender sees it in their
	// activity feed; target's inbox naturally surfaces the message.
	h.events.Emit(MCPEvent{
		Type:    "message",
		Action:  "cross_project",
		Agent:   from,
		Project: srcProject,
		Target:  fmt.Sprintf("%s@%s", to, dstProject),
		Label:   subject,
	})

	return h.resultJSONTracked(srcProject, from, "send_message", msg)
}

func (h *Handlers) notifyConversation(project, conversationID, senderName, subject, messageID string) {
	members, err := h.db.GetConversationMembers(conversationID)
	if err != nil {
		return
	}
	for _, m := range members {
		if m.AgentName != senderName {
			h.registry.Notify(project, m.AgentName, senderName, subject, messageID)
		}
	}
}

// resolveProject returns the project from the explicit `project` tool parameter,
// falling back to the HTTP context default (from ?project= URL param).
func resolveProject(ctx context.Context, req mcp.CallToolRequest) string {
	if p := req.GetString("project", ""); p != "" {
		return p
	}
	return ProjectFromContext(ctx)
}

// resolveAgent returns the agent name from the explicit `as` tool parameter,
// falling back to the HTTP context default (from ?agent= URL param).
// Names are lowercased for case-insensitive matching.
func resolveAgent(ctx context.Context, req mcp.CallToolRequest) string {
	if as := req.GetString("as", ""); as != "" {
		return strings.ToLower(as)
	}
	return AgentFromContext(ctx)
}

// resolveTaskID resolves a potentially short task ID prefix to a full UUID.
func (h *Handlers) resolveTaskID(taskID, project string) (string, error) {
	return h.db.ResolveTaskID(taskID, project)
}

// helpers

func resultJSON(data any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("json marshal: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optionalStringLower(s string) *string {
	if s == "" {
		return nil
	}
	l := strings.ToLower(s)
	return &l
}

// normalizeJSONArrayParam handles profile parameters that can be either a JSON string
// (e.g. "[\"a\",\"b\"]") or a native JSON array from the MCP client. Returns a JSON string.
func normalizeJSONArrayParam(req mcp.CallToolRequest, key string) string {
	// First try as string (the documented format)
	if s := req.GetString(key, ""); s != "" {
		// Validate it's valid JSON
		var check json.RawMessage
		if json.Unmarshal([]byte(s), &check) == nil {
			return s
		}
		// Not valid JSON — wrap as a single-element array
		b, _ := json.Marshal([]string{s})
		return string(b)
	}
	// Try to extract the raw argument value — it might be a native array
	if args := req.GetArguments(); args != nil {
		if raw, exists := args[key]; exists {
			// Re-marshal whatever the MCP client sent (array, object, etc.)
			b, err := json.Marshal(raw)
			if err == nil {
				return string(b)
			}
		}
	}
	return "[]"
}

// mapPriority normalizes MACP aliases to P0-P3.
func mapPriority(p string) string {
	switch strings.ToLower(p) {
	case "interrupt", "p0":
		return "P0"
	case "steering", "p1":
		return "P1"
	case "advisory", "p2", "":
		return "P2"
	case "info", "p3":
		return "P3"
	default:
		return "P2"
	}
}

func sessionFromContext(ctx context.Context) clientSession {
	if sess, ok := ctx.Value(sessionKey).(clientSession); ok {
		return sess
	}
	return nil
}

type clientSession interface {
	SessionID() string
}

const sessionKey contextKey = "mcp_session"

// --- Memory handlers ---

// --- Profile handlers ---

// --- Task handlers ---

// --- File locks ---

// findExistingClaims returns claims held by other agents that overlap with the
// requested paths. Used to decorate claim_files responses with a conflict hint.
func (h *Handlers) findExistingClaims(project, selfAgent string, requested []string) []map[string]any {
	if len(requested) == 0 {
		return nil
	}
	want := make(map[string]bool, len(requested))
	for _, p := range requested {
		want[p] = true
	}
	locks, err := h.db.ListFileLocks(project)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, l := range locks {
		if l.AgentName == selfAgent {
			continue
		}
		var paths []string
		_ = json.Unmarshal([]byte(l.FilePaths), &paths)
		var overlap []string
		for _, p := range paths {
			if want[p] {
				overlap = append(overlap, p)
			}
		}
		if len(overlap) > 0 {
			out = append(out, map[string]any{
				"agent":       l.AgentName,
				"overlapping": overlap,
				"claimed_at":  l.ClaimedAt,
			})
		}
	}
	return out
}

// --- Agent lifecycle ---

func buildOnboardingPrompt(name, description, cwd string, interactive bool) string {
	var b strings.Builder

	b.WriteString("# Colony Setup — " + name + "\n\n")
	b.WriteString("You are the **setup agent** for project `" + name + "` on the Agent Relay. ")
	b.WriteString("Your job is to configure the entire relay infrastructure so multi-agent work can begin. ")
	b.WriteString("Think of this like founding a colony in a management game — you place the buildings, assign roles, and set objectives before the workers arrive.\n\n")

	if interactive {
		b.WriteString("**MODE: INTERACTIVE** — At the end of each phase, present your findings and proposed actions to the user. Wait for their approval before executing. Do NOT create anything without confirmation.\n\n")
	} else {
		b.WriteString("**Before starting**, ask the user:\n\n")
		b.WriteString("> **Auto mode** or **Interactive mode**?\n")
		b.WriteString("> - **Auto**: I execute everything autonomously. You get a summary at the end.\n")
		b.WriteString("> - **Interactive**: I present my findings at each phase and wait for your approval before creating anything.\n\n")
		b.WriteString("If the user picks interactive, add CHECKPOINT pauses after Phase 2 (present analysis), before Phase 3 (approve vault docs), before Phase 5 (approve teams/profiles), and before Phase 8 (approve sprint tasks). At each checkpoint, present what you plan to do and wait for confirmation.\n\n")
		b.WriteString("If the user picks auto (or says nothing after 10 seconds), execute everything in order without stopping.\n\n")
	}

	if description != "" {
		b.WriteString("**Project description:** " + description + "\n\n")
	}
	if cwd != "" {
		b.WriteString("**Project root:** `" + cwd + "`\n\n")
	}

	b.WriteString("---\n\n")

	// Phase 1 — Learn the relay
	b.WriteString("## Phase 1 — Learn the relay\n\n")
	b.WriteString("The relay embeds its own documentation. Read it first:\n\n")
	b.WriteString("```\n")
	b.WriteString("search_vault({ query: \"boot sequence\", project: \"_relay\" })\n")
	b.WriteString("search_vault({ query: \"profiles vault_paths soul_keys\", project: \"_relay\" })\n")
	b.WriteString("search_vault({ query: \"memory scopes layers\", project: \"_relay\" })\n")
	b.WriteString("search_vault({ query: \"teams permissions\", project: \"_relay\" })\n")
	b.WriteString("search_vault({ query: \"task dispatch boards\", project: \"_relay\" })\n")
	b.WriteString("```\n\n")
	b.WriteString("Read the results carefully. This is how the system works.\n\n")

	b.WriteString("---\n\n")

	// Phase 2 — Analyze the codebase
	b.WriteString("## Phase 2 — Analyze the codebase\n\n")
	b.WriteString("Thoroughly explore the project to understand:\n\n")
	b.WriteString("- **Domain**: What does this project do? Who is it for?\n")
	b.WriteString("- **Tech stack**: Languages, frameworks, databases, package manager, runtime\n")
	b.WriteString("- **Architecture**: Monorepo? Microservices? Key modules and data flow\n")
	b.WriteString("- **Conventions**: Naming, code style, commit format, testing approach\n")
	b.WriteString("- **Infrastructure**: Hosting, CI/CD, env vars, deployment\n")
	b.WriteString("- **Auth**: How authentication works (if applicable)\n")
	b.WriteString("- **API**: REST/GraphQL/tRPC patterns (if applicable)\n\n")
	b.WriteString("Read at minimum: main entry point, package manifest (package.json / go.mod / Cargo.toml / etc.), config files, README, and 3-5 core source files.\n\n")
	b.WriteString("Write down your findings — you store them as memories in Phase 4.\n\n")

	if interactive {
		b.WriteString("**CHECKPOINT:** Present your findings to the user:\n")
		b.WriteString("- Domain summary\n- Tech stack with versions\n- Architecture overview\n- Key conventions\n")
		b.WriteString("- Proposed project name (if different from `" + name + "`)\n")
		b.WriteString("- List of teams you plan to create\n- List of profiles you plan to register\n\n")
		b.WriteString("Wait for approval before continuing.\n\n")
	}

	b.WriteString("---\n\n")

	// Phase 3 — Create the vault
	b.WriteString("## Phase 3 — Create the vault\n\n")
	b.WriteString("Create an Obsidian-compatible vault **next to** the repo (not inside it):\n\n")
	b.WriteString("```bash\nmkdir -p ../obsidian/" + name + "\n```\n\n")
	b.WriteString("Write markdown docs based on your analysis:\n\n")
	b.WriteString("| File | Content |\n|------|---------|\n")
	b.WriteString("| `architecture.md` | System overview, module map, data flow |\n")
	b.WriteString("| `stack.md` | Full tech stack with versions |\n")
	b.WriteString("| `conventions.md` | Code style, naming, commit format, testing |\n")
	b.WriteString("| `api.md` | Endpoints, protocols, session lifecycle (if applicable) |\n")
	b.WriteString("| `env.md` | Required env vars (names only, never values) |\n\n")
	b.WriteString("Then register it with the relay:\n\n")
	b.WriteString("```\nregister_vault({ path: \"<absolute-path-to-vault>\", project: \"" + name + "\" })\n```\n\n")

	b.WriteString("---\n\n")

	// Phase 4 — Store project knowledge
	b.WriteString("## Phase 4 — Store project knowledge\n\n")
	b.WriteString("Use `set_memory` to persist what you learned. All memories use `scope: \"project\"`, `project: \"" + name + "\"`.\n\n")
	b.WriteString("**Required memories:**\n\n")
	b.WriteString("| Key | Layer | Tags | Content |\n|-----|-------|------|---------|\n")
	b.WriteString("| `stack` | constraints | `[\"stack\", \"tech\"]` | Languages, frameworks, versions |\n")
	b.WriteString("| `architecture` | constraints | `[\"architecture\", \"system\"]` | High-level structure, modules, data flow |\n")
	b.WriteString("| `conventions` | behavior | `[\"conventions\", \"style\"]` | Naming, style, commits, testing |\n")
	b.WriteString("| `domain` | constraints | `[\"domain\", \"product\"]` | What the product does, target users |\n")
	b.WriteString("| `infra` | behavior | `[\"infra\", \"hosting\"]` | Hosting, CI, databases, deployment |\n\n")
	b.WriteString("**Optional** (add if relevant): `auth-pattern`, `api-pattern`, `db-schema-overview`, `env-vars`\n\n")
	b.WriteString("Use `confidence: \"observed\"` since you read the codebase directly.\n\n")

	b.WriteString("---\n\n")

	if interactive {
		b.WriteString("**CHECKPOINT:** Show the user the vault docs and memories you plan to create. Wait for approval.\n\n")
		b.WriteString("---\n\n")
	}

	// Phase 5 — Create the org
	b.WriteString("## Phase 5 — Create the org\n\n")
	b.WriteString("### 5a. Teams\n\n")
	b.WriteString("Create teams based on what you discovered in Phase 2. The leadership team is always required:\n\n")
	b.WriteString("```\n")
	b.WriteString("create_team({ name: \"Leadership\", slug: \"leadership\", type: \"admin\", description: \"Executive team — broadcast and cross-team coordination\", project: \"" + name + "\" })\n")
	b.WriteString("```\n\n")
	b.WriteString("Then create **only the teams that match the actual codebase**. Examples:\n")
	b.WriteString("- Go API → `backend` team only\n")
	b.WriteString("- Next.js fullstack → `backend` + `frontend`\n")
	b.WriteString("- Monorepo with infra → `backend` + `frontend` + `infra`\n")
	b.WriteString("- Python ML project → `backend` + `data`\n")
	b.WriteString("- CLI tool → `core` team only\n\n")
	b.WriteString("Do NOT create teams for parts of the stack that don't exist.\n\n")

	b.WriteString("### 5b. Profiles\n\n")
	b.WriteString("Register role archetypes based on the teams you created. **Derive profiles from your Phase 2 analysis, not from a template.**\n\n")
	b.WriteString("The **CTO** profile is always required:\n```\n")
	b.WriteString("register_profile({\n")
	b.WriteString("  slug: \"cto\",\n  name: \"CTO\",\n")
	b.WriteString("  role: \"Technical leader. Owns the backlog, sets priorities, coordinates all teams, reviews architecture.\",\n")
	b.WriteString("  context_pack: \"You are the CTO of " + name + ". You make architecture decisions, manage the task board, and coordinate between tech leads. You have broadcast permissions.\",\n")
	b.WriteString("  skills: \"[{\\\"id\\\":\\\"architecture\\\",\\\"name\\\":\\\"System Architecture\\\",\\\"tags\\\":[\\\"architecture\\\",\\\"design\\\"]},{\\\"id\\\":\\\"management\\\",\\\"name\\\":\\\"Technical Management\\\",\\\"tags\\\":[\\\"management\\\",\\\"coordination\\\"]}]\",\n")
	b.WriteString("  soul_keys: \"[\\\"stack\\\",\\\"architecture\\\",\\\"domain\\\",\\\"conventions\\\",\\\"infra\\\"]\",\n")
	b.WriteString("  vault_paths: \"[\\\"architecture.md\\\",\\\"stack.md\\\"]\",\n")
	b.WriteString("  project: \"" + name + "\"\n})\n```\n\n")
	b.WriteString("For each additional team, create **one tech lead profile**. Use the actual stack in skills/context_pack:\n```\n")
	b.WriteString("register_profile({\n")
	b.WriteString("  slug: \"<team-slug>-lead\",\n  name: \"<Team> Tech Lead\",\n")
	b.WriteString("  role: \"<what this role does, based on the actual codebase>\",\n")
	b.WriteString("  context_pack: \"You are the <role> for " + name + ". <specific responsibilities based on what you found>\",\n")
	b.WriteString("  skills: \"<JSON array — use ACTUAL languages/frameworks/tools from Phase 2>\",\n")
	b.WriteString("  soul_keys: \"<JSON array — pick relevant memory keys>\",\n")
	b.WriteString("  vault_paths: \"<JSON array — pick relevant vault docs>\",\n")
	b.WriteString("  project: \"" + name + "\"\n})\n```\n\n")
	b.WriteString("**Rules:**\n")
	b.WriteString("- Only create profiles for teams that exist\n")
	b.WriteString("- Skills must reference the real tech stack (e.g. \"Go 1.22\", \"SQLite\", \"React 19\"), never generic placeholders\n")
	b.WriteString("- A Go-only project gets 0 frontend profiles. A fullstack project gets both. Use judgment.\n\n")

	b.WriteString("### 5c. Register yourself as CTO\n\n")
	b.WriteString("```\nwhoami({ salt: \"<generate-3-random-words>\" })\n```\n\n")
	b.WriteString("Then:\n```\n")
	b.WriteString("register_agent({\n")
	b.WriteString("  name: \"cto\",\n  project: \"" + name + "\",\n")
	b.WriteString("  role: \"Technical leader and architect. Owns the backlog, coordinates teams.\",\n")
	b.WriteString("  is_executive: true,\n  profile_slug: \"cto\",\n")
	b.WriteString("  session_id: \"<session_id from whoami>\"\n})\n```\n\n")

	b.WriteString("### 5d. Team memberships\n\n")
	b.WriteString("Add the CTO to **every functional team you created** as admin:\n\n")
	b.WriteString("```\nadd_team_member({ team: \"<team-slug>\", agent_name: \"cto\", role: \"admin\", project: \"" + name + "\" })\n```\n\n")
	b.WriteString("Repeat for each team from 5a (not leadership — the CTO is already there via auto-admin).\n\n")

	b.WriteString("---\n\n")

	// Phase 6 — Set up the board
	b.WriteString("## Phase 6 — Set up the board\n\n")
	b.WriteString("### 6a. Backlog board\n\n")
	b.WriteString("```\ncreate_board({ name: \"Backlog\", slug: \"backlog\", description: \"Main task board\", project: \"" + name + "\" })\n```\n\n")

	b.WriteString("---\n\n")

	// Phase 7 — Verify & spawn
	b.WriteString("## Phase 7 — Verify & spawn workers\n\n")
	b.WriteString("### 7a. Verify everything\n\n")
	b.WriteString("Run checks:\n\n")
	b.WriteString("```\n")
	b.WriteString("list_agents({ project: \"" + name + "\" })\n")
	b.WriteString("list_teams({ project: \"" + name + "\" })\n")
	b.WriteString("list_profiles({ project: \"" + name + "\" })\n")
	b.WriteString("list_boards({ project: \"" + name + "\" })\n")
	b.WriteString("```\n\n")

	b.WriteString("### 7b. Spawn worker commands\n\n")
	b.WriteString("For **each non-CTO profile** you created, output a ready-to-paste `claude` command.\n\n")
	b.WriteString("Use this exact template for each worker (replace `<SLUG>`, `<ROLE>`, `<NAME>`):\n\n")
	b.WriteString("```bash\nclaude -w --dangerously-skip-permissions \\\n")
	b.WriteString("  \"You are the <ROLE> of " + name + ". Boot sequence:\n")
	b.WriteString("  1. register_agent({ name: '<SLUG>', project: '" + name + "', profile_slug: '<SLUG>', reports_to: 'cto' })\n")
	b.WriteString("  2. get_session_context() — read everything: profile, vault docs, memories, tasks\n")
	b.WriteString("  3. Research the technologies and patterns mentioned in your context using web search. Get up to speed.\n")
	b.WriteString("  4. set_memory() to persist your research findings in the relay (scope: 'agent', key: 'onboarding-research')\n")
	b.WriteString("  5. send_message({ to: 'cto', type: 'notification', subject: 'Ready', content: '<NAME> onboarded and ready for tasks.' })\n")
	b.WriteString("  6. Check your inbox and the board. Start working.\"\n")
	b.WriteString("```\n\n")
	b.WriteString("Output one command per profile. The user copies each into a separate terminal.\n\n")

	b.WriteString("### 7c. Report\n\n")
	b.WriteString("Summarize to the user:\n\n")
	b.WriteString("- **Project**: name, planet type\n")
	b.WriteString("- **Vault**: path, docs indexed\n")
	b.WriteString("- **Memories**: keys stored\n")
	b.WriteString("- **Teams**: list with types\n")
	b.WriteString("- **Profiles**: list with roles\n")
	b.WriteString("- **Board**: ready for tasks\n")
	b.WriteString("- **CTO**: registered, executive, broadcast enabled\n")
	b.WriteString("- **Spawn commands**: listed above, ready to paste\n\n")
	b.WriteString("---\n\n")

	// Phase 8 — Sprint planning
	b.WriteString("## Phase 8 — Plan the first two sprints\n\n")
	b.WriteString("Now that the colony is configured, plan the work.\n\n")
	b.WriteString("Based on the codebase analysis and current state of the project:\n\n")
	b.WriteString("### Sprint 1 (immediate priorities)\n")
	b.WriteString("Create 3-6 tasks for the most impactful work to do right now:\n\n")
	b.WriteString("```\ndispatch_task({\n  title: \"<task title>\",\n  description: \"<what to do and acceptance criteria>\",\n  profile: \"<profile-slug>\",\n  priority: \"<p0-p3>\",\n  board_id: \"<backlog board ID>\",\n  project: \"" + name + "\"\n})\n```\n\n")
	b.WriteString("### Sprint 2 (next up)\n")
	b.WriteString("Create 3-6 more tasks for the next wave of work. These can depend on Sprint 1 outputs.\n\n")
	b.WriteString("Assign profiles based on the skills needed. Distribute work across teams — don't overload one profile.\n\n")

	if interactive {
		b.WriteString("**CHECKPOINT:** Present the sprint plan to the user before dispatching tasks. Wait for approval.\n\n")
	}

	b.WriteString("---\n\n")
	b.WriteString("**The colony is ready.** Paste the spawn commands from Phase 7 in separate terminals to deploy your workers. They will pick up tasks from the board automatically.\n")

	return b.String()
}

// --- Find profiles by skill ---

// --- Session context ---

func (h *Handlers) buildSessionContext(project, agentName string, profileSlug *string) map[string]any {
	result := map[string]any{}

	// Profile
	if profileSlug != nil && *profileSlug != "" {
		profile, err := h.db.GetProfile(project, *profileSlug)
		if err == nil && profile != nil {
			result["profile"] = profile
		}
	}

	// Tasks — project through paper's Def. 7 (Budget Projection) to bound
	// session_context payload. Full task bodies are fetched via get_task(id).
	assignedToMe, dispatchedByMe, _ := h.db.GetAgentTasks(project, agentName)
	if assignedToMe == nil {
		assignedToMe = []models.Task{}
	}
	if dispatchedByMe == nil {
		dispatchedByMe = []models.Task{}
	}
	result["pending_tasks"] = map[string]any{
		"assigned_to_me":   projectTasks(assignedToMe, 8000),
		"dispatched_by_me": projectTasks(dispatchedByMe, 3000),
	}

	// Unread messages — projected through Def. 7 so verbose alert bodies
	// (Prometheus/GlitchTip digests ~4k chars each) can't blow up the boot
	// payload. Content is preview-truncated; full bodies via get_inbox(full_content=true).
	unread, err := h.db.GetInbox(project, agentName, true, 50)
	if err != nil || unread == nil {
		unread = []models.Message{}
	}
	projected := projectMessages(unread, sessionUnreadBudget)
	result["unread_messages"] = projected
	if len(projected) < len(unread) {
		result["unread_omitted"] = len(unread) - len(projected)
	}

	// Active conversations
	convs, err := h.db.ListConversations(project, agentName)
	if err != nil || convs == nil {
		convs = []models.ConversationSummary{}
	}
	result["active_conversations"] = convs

	// Relevant memories — cross-scope boot view (global + project + own
	// agent-scope; plain ListMemories filters agent_name on all scopes and
	// would hide other agents' shared memories), projected through Def. 7
	// with constraints-layer bypass. Full values via get_memory(key).
	memories, err := h.db.ListBootMemories(project, agentName, 50)
	if err != nil || memories == nil {
		memories = []models.Memory{}
	}
	projectedMems := projectMemories(memories, sessionMemoryBudget)
	result["relevant_memories"] = projectedMems
	if len(projectedMems) < len(memories) {
		result["memories_omitted"] = len(memories) - len(projectedMems)
	}

	// Vault/doc context is served externally (ctx.prod.synergix.ch), not injected here.

	return result
}

// --- Soul RAG ---

// --- Teams + Orgs Handlers ---
