package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	connector connector.TaskConnector
}

// SetNotifier connects the notifications subsystem so handlers can emit custom
// events into the rules engine.
func (h *Handlers) SetNotifier(n *Notifier) {
	h.notifier = n
}

// SetConnector wires the task connector so the review handler can fire the one
// owned write-back (→ In Review + comment) when running in Linear mode.
func (h *Handlers) SetConnector(c connector.TaskConnector) {
	h.connector = c
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

// HandleWhoami finds the caller's Claude Code session by grepping transcripts for a unique salt.
func (h *Handlers) HandleWhoami(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	salt := req.GetString("salt", "")
	if salt == "" {
		return mcp.NewToolResultError("salt is required"), nil
	}
	if len(salt) < 5 {
		return mcp.NewToolResultError("salt too short — use at least 3 random words"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return mcp.NewToolResultError("cannot determine home dir"), nil
	}

	claudeDir := filepath.Join(home, ".claude", "projects")
	var matchFile string

	// Walk all .jsonl transcript files
	_ = filepath.Walk(claudeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if matchFile != "" {
			return filepath.SkipAll
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		// Scan from the end — salt is in recent lines. Read last 64KB.
		stat, _ := f.Stat()
		offset := stat.Size() - 65536
		if offset < 0 {
			offset = 0
		}
		_, _ = f.Seek(offset, 0)

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), salt) {
				matchFile = path
				return filepath.SkipAll
			}
		}
		return nil
	})

	if matchFile == "" {
		return mcp.NewToolResultError("salt not found in any transcript — make sure you wrote the salt in your conversation before calling whoami"), nil
	}

	// Extract session ID from filename (UUID.jsonl)
	base := filepath.Base(matchFile)
	sessionID := strings.TrimSuffix(base, ".jsonl")

	return resultJSON(map[string]any{
		"session_id":      sessionID,
		"transcript_path": matchFile,
	})
}

func (h *Handlers) HandleRegisterAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	name := strings.ToLower(req.GetString("name", ""))
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	role := req.GetString("role", "")
	description := req.GetString("description", "")
	reportsTo := optionalStringLower(req.GetString("reports_to", ""))
	profileSlug := optionalString(req.GetString("profile_slug", ""))
	isExecutive := req.GetBool("is_executive", false)
	sessionID := optionalString(req.GetString("session_id", ""))
	interestTags := req.GetString("interest_tags", "[]")
	maxContextBytes := req.GetInt("max_context_bytes", 16384)

	agent, isRespawn, err := h.db.RegisterAgent(project, name, role, description, reportsTo, profileSlug, isExecutive, sessionID, interestTags, maxContextBytes)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to register agent: %v", err)), nil
	}

	// Auto-create admin team + add executive agent (fixes broadcast permission UX)
	var autoAdminTeam *string
	if isExecutive {
		adminTeam, _ := h.db.GetTeam(project, "leadership")
		if adminTeam == nil {
			adminTeam, _ = h.db.CreateTeam("Leadership", "leadership", project, "Auto-created admin team for executive agents", "admin", nil, nil)
		}
		if adminTeam != nil {
			_ = h.db.AddTeamMember(adminTeam.ID, name, project, "admin")
			autoAdminTeam = &adminTeam.Slug
		}
	}

	// Register the session for push notifications
	if sess := sessionFromContext(ctx); sess != nil {
		h.registry.Register(project, name, sess.SessionID())
	}

	// Build session_context for the response (Phase 2: boot-in-register)
	sessionCtx := h.buildSessionContext(project, name, profileSlug)
	sessionCtx["is_respawn"] = isRespawn

	action := "register"
	if isRespawn {
		action = "respawn"
	}
	h.events.Emit(MCPEvent{Type: "register", Action: action, Agent: name, Project: project, Label: role})

	resp := map[string]any{
		"agent":           agent,
		"session_context": sessionCtx,
	}
	if autoAdminTeam != nil {
		resp["auto_admin_team"] = *autoAdminTeam
		resp["hint"] = "You were auto-added to the 'leadership' admin team (broadcast enabled). Use send_message(to='*') to broadcast."
	}
	return h.resultJSONTracked(project, name, "register_agent", resp)
}

func (h *Handlers) HandleSendMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	from := resolveAgent(ctx, req)
	to := strings.ToLower(req.GetString("to", ""))
	msgType := req.GetString("type", "notification")
	subject := req.GetString("subject", "")
	content := req.GetString("content", "")
	if content == "" {
		return mcp.NewToolResultError("content is required"), nil
	}

	metadata := req.GetString("metadata", "{}")
	replyTo := optionalString(req.GetString("reply_to", ""))
	conversationID := optionalString(req.GetString("conversation_id", ""))
	priority := mapPriority(req.GetString("priority", "P2"))
	ttlSeconds := req.GetInt("ttl_seconds", 14400)
	targetProject := strings.TrimSpace(req.GetString("target_project", ""))

	// Quota check: messages
	if qErr := h.db.CheckQuotaError(project, from, "messages"); qErr != "" {
		return mcp.NewToolResultError(qErr), nil
	}

	// Cross-project DM: delivered to a peer executive in a different project.
	// MVP scope: direct messages only (no broadcast, no team, no conversation).
	// Both sender and recipient must be registered with is_executive=true.
	if targetProject != "" && targetProject != project {
		if to == "" {
			return mcp.NewToolResultError("target_project requires a 'to' agent name"), nil
		}
		if to == "*" || strings.HasPrefix(to, "team:") || conversationID != nil {
			return mcp.NewToolResultError("cross-project messaging is limited to direct DMs (no broadcast, no team:, no conversation_id)"), nil
		}
		return h.sendCrossProject(ctx, project, from, targetProject, to, msgType, subject, content, metadata, replyTo, priority, ttlSeconds)
	}

	// Support "to": "conversation:<id>" shorthand
	if conversationID == nil && strings.HasPrefix(to, "conversation:") {
		cid := strings.TrimPrefix(to, "conversation:")
		conversationID = &cid
	}

	// Touch sender's last_seen
	_ = h.db.TouchAgent(project, from)

	if conversationID != nil {
		// Conversation message — validate membership
		isMember, err := h.db.IsConversationMember(*conversationID, from)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to check membership: %v", err)), nil
		}
		if !isMember {
			return mcp.NewToolResultError("you are not a member of this conversation"), nil
		}
		to = "" // no single recipient for conversation messages
	} else if to == "" {
		return mcp.NewToolResultError("to is required (or provide conversation_id)"), nil
	}

	// Permission check: only enforce when teams are configured (bypass for "user" — always reachable)
	if conversationID == nil && to != "*" && to != "user" && !strings.HasPrefix(to, "team:") {
		hasTeams, _ := h.db.HasTeams(project)
		if hasTeams {
			allowed, err := h.db.CanMessage(project, from, to)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("permission check failed: %v", err)), nil
			}
			if !allowed {
				return mcp.NewToolResultError(fmt.Sprintf("not authorized to message '%s' — no shared team, reports_to chain, or notify channel", to)), nil
			}
		}
	}

	// Team addressing: to="team:slug" → fan out to team members + team_inbox
	if strings.HasPrefix(to, "team:") {
		teamSlug := strings.TrimPrefix(to, "team:")
		team, err := h.db.ResolveTeamSlug(project, teamSlug)
		if err != nil || team == nil {
			return mcp.NewToolResultError(fmt.Sprintf("team '%s' not found", teamSlug)), nil
		}

		msg, err := h.db.InsertMessage(project, from, to, msgType, subject, content, metadata, priority, ttlSeconds, replyTo, conversationID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to send message: %v", err)), nil
		}

		// Add to team inbox
		_ = h.db.AddToTeamInbox(team.ID, msg.ID)

		// Create deliveries for team members
		members, _ := h.db.GetTeamMemberNames(team.ID)
		var recipients []string
		for _, member := range members {
			if member != from {
				recipients = append(recipients, member)
				h.registry.Notify(project, member, from, subject, msg.ID)
			}
		}
		_ = h.db.CreateDeliveries(msg.ID, project, recipients)

		return h.resultJSONTracked(project, from, "send_message", msg)
	}

	// Broadcast permission: when teams exist, only admin team members can broadcast
	if to == "*" {
		hasTeams, _ := h.db.HasTeams(project)
		if hasTeams {
			allowed, _ := h.db.CanMessage(project, from, "*")
			if !allowed {
				return mcp.NewToolResultError("broadcast requires membership in an 'admin' type team. Fix: register with is_executive=true (auto-creates admin team), or manually: create_team(type='admin') then add_team_member()"), nil
			}
		}
	}

	msg, err := h.db.InsertMessage(project, from, to, msgType, subject, content, metadata, priority, ttlSeconds, replyTo, conversationID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to send message: %v", err)), nil
	}

	// Create deliveries
	recipients, _ := h.db.ResolveRecipients(project, to, from, conversationID)
	_ = h.db.CreateDeliveries(msg.ID, project, recipients)

	// Push notification
	if conversationID != nil {
		h.notifyConversation(project, *conversationID, from, subject, msg.ID)
	} else if to == "*" {
		h.registry.NotifyBroadcast(project, from, subject, msg.ID)
	} else {
		h.registry.Notify(project, to, from, subject, msg.ID)
	}

	// Emit visual event for activity feed / SSE subscribers. Action distinguishes
	// broadcast / team / conversation / direct so the UI can render icons.
	action := "send"
	switch {
	case to == "*":
		action = "broadcast"
	case strings.HasPrefix(to, "team:"):
		action = "team"
	case conversationID != nil:
		action = "conversation"
	}
	h.events.Emit(MCPEvent{Type: "message", Action: action, Agent: from, Project: project, Target: to, Label: subject})

	return h.resultJSONTracked(project, from, "send_message", msg)
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
	msg, err := h.db.InsertMessage(dstProject, from, to, msgType, subject, content, string(metaBytes), priority, ttlSeconds, replyTo, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to insert cross-project message: %v", err)), nil
	}
	// Delivery for the single recipient in the destination project
	if err := h.db.CreateDeliveries(msg.ID, dstProject, []string{to}); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delivery failed: %v", err)), nil
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

func (h *Handlers) HandleGetInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	unreadOnly := req.GetBool("unread_only", true)
	limit := req.GetInt("limit", 10)
	fullContent := req.GetBool("full_content", false)
	budgetMode := req.GetBool("apply_budget", false)

	_ = h.db.TouchAgent(project, agent)

	// Expire stale messages before querying
	_, _ = h.db.ExpireMessages()

	// Build inbox filters
	filter := db.InboxFilter{
		MinPriority:       req.GetString("min_priority", ""),
		From:              req.GetString("from", ""),
		Since:             req.GetString("since", ""),
		ExcludeBroadcasts: req.GetBool("exclude_broadcasts", false),
	}

	// Budget mode needs a 2-step flow: fetch without surfacing, prune, then surface
	// only the survivors. Otherwise messages dropped by the budget filter would be
	// marked 'surfaced' and invisible on the next poll.
	var (
		messages []models.Message
		err      error
	)
	if budgetMode && h.db.HasDeliveries() {
		messages, err = h.db.FetchInboxDeliveries(project, agent, unreadOnly, limit, filter)
	} else {
		messages, err = h.db.GetInbox(project, agent, unreadOnly, limit, filter)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get inbox: %v", err)), nil
	}
	if messages == nil {
		messages = []models.Message{}
	}

	// Apply context budget pruning if requested
	if budgetMode && len(messages) > 0 {
		agentObj, _ := h.db.GetAgent(project, agent)
		if agentObj != nil {
			var tags []string
			_ = json.Unmarshal([]byte(agentObj.InterestTags), &tags)
			messages = applyBudget(messages, tags, agentObj.MaxContextBytes)
		}
		// Surface only the deliveries that survived the budget filter
		var surviving []string
		for _, m := range messages {
			if m.DeliveryID != nil && m.DeliveryState != nil && *m.DeliveryState == "queued" {
				surviving = append(surviving, *m.DeliveryID)
			}
		}
		h.db.MarkDeliveriesSurfaced(surviving)
	}

	formatted := make([]map[string]any, len(messages))
	for i, m := range messages {
		content := m.Content
		if !fullContent && len(content) > 300 {
			content = content[:300] + "..."
		}
		entry := map[string]any{
			"id":         m.ID,
			"from":       m.From,
			"to":         m.To,
			"type":       m.Type,
			"subject":    m.Subject,
			"content":    content,
			"created_at": m.CreatedAt,
			"priority":   m.Priority,
		}
		if m.ReplyTo != nil {
			entry["reply_to"] = *m.ReplyTo
		}
		if m.ConversationID != nil {
			entry["conversation_id"] = *m.ConversationID
		}
		if m.DeliveryID != nil {
			entry["delivery_id"] = *m.DeliveryID
		}
		if m.DeliveryState != nil {
			entry["delivery_state"] = *m.DeliveryState
		}
		// Surface cross-project origin when present so the caller (and UI)
		// can render "from X@colony-b" instead of a bare sender name.
		if m.Metadata != "" && m.Metadata != "{}" {
			var meta map[string]any
			if err := json.Unmarshal([]byte(m.Metadata), &meta); err == nil {
				if sp, ok := meta["source_project"].(string); ok && sp != "" {
					entry["source_project"] = sp
				}
				if sa, ok := meta["source_agent"].(string); ok && sa != "" {
					entry["source_agent"] = sa
				}
				if cp, ok := meta["cross_project"].(bool); ok && cp {
					entry["cross_project"] = true
				}
			}
		}
		formatted[i] = entry
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(messages))
		for i, m := range messages {
			content, _ := formatted[i]["content"].(string)
			// Threading context: conversation membership or reply chain.
			thread := ""
			if m.ConversationID != nil {
				thread = "conv:" + *m.ConversationID
			} else if m.ReplyTo != nil {
				thread = "re:" + *m.ReplyTo
			}
			rows[i] = []string{
				m.ID, strOrDash(m.DeliveryID), m.From, m.To, m.Type,
				m.Priority, thread, m.CreatedAt, m.Subject, content,
			}
		}
		table := renderTable([]string{"id", "delivery_id", "from", "to", "type", "priority", "thread", "created_at", "subject", "content"}, rows)
		return h.resultTextTracked(project, agent, "get_inbox", fmt.Sprintf("%d messages for %s\n%s", len(messages), agent, table))
	}

	return h.resultJSONTracked(project, agent, "get_inbox", map[string]any{
		"agent":    agent,
		"count":    len(messages),
		"messages": formatted,
	})
}

func (h *Handlers) HandleAckDelivery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	deliveryID := req.GetString("delivery_id", "")
	if deliveryID == "" {
		// Common mistake: callers pass message_id. Offer a fallback if the
		// caller also passed as+project so we can resolve message_id → delivery_id.
		if msgID := req.GetString("message_id", ""); msgID != "" {
			agent := resolveAgent(ctx, req)
			project := resolveProject(ctx, req)
			if err := h.db.AcknowledgeDeliveryByMessage(msgID, agent, project); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to acknowledge delivery by message: %v", err)), nil
			}
			return h.resultJSONTracked(project, agent, "ack_delivery", map[string]any{"acknowledged_message_id": msgID})
		}
		return mcp.NewToolResultError("delivery_id is required (get it from get_inbox response — each message has a delivery_id field). If you only have the message_id, pass message_id + as + project instead."), nil
	}
	if err := h.db.AcknowledgeDelivery(deliveryID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to acknowledge delivery: %v", err)), nil
	}
	return h.resultJSONTracked(resolveProject(ctx, req), "", "ack_delivery", map[string]any{"acknowledged": deliveryID})
}

func (h *Handlers) HandleGetThread(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	messageID := req.GetString("message_id", "")
	if messageID == "" {
		return mcp.NewToolResultError("message_id is required"), nil
	}

	messages, err := h.db.GetThread(messageID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get thread: %v", err)), nil
	}

	formatted := make([]map[string]any, len(messages))
	for i, m := range messages {
		entry := map[string]any{
			"id":         m.ID,
			"from":       m.From,
			"to":         m.To,
			"type":       m.Type,
			"subject":    m.Subject,
			"content":    m.Content,
			"created_at": m.CreatedAt,
			"priority":   m.Priority,
		}
		if m.ReplyTo != nil {
			entry["reply_to"] = *m.ReplyTo
		}
		if m.ConversationID != nil {
			entry["conversation_id"] = *m.ConversationID
		}
		if m.Metadata != "" && m.Metadata != "{}" {
			entry["metadata"] = m.Metadata
		}
		formatted[i] = entry
	}

	return h.resultJSONTracked(resolveProject(ctx, req), "", "get_thread", map[string]any{
		"count":    len(formatted),
		"messages": formatted,
	})
}

func (h *Handlers) HandleListAgents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)

	agents, err := h.db.ListAgents(project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list agents: %v", err)), nil
	}
	if agents == nil {
		agents = []models.Agent{}
	}

	// Enrich with live activity from ingester
	var sessions []ingest.SessionState
	if h.ingester != nil {
		sessions = h.ingester.GetSessions()
	}
	sessionByID := make(map[string]ingest.SessionState)
	for _, s := range sessions {
		sessionByID[s.SessionID] = s
	}

	// Curated list view: identity + liveness only. Full records (session_id,
	// interest_tags, max_context_bytes, timestamps) stay on the REST API.
	type agentEntry struct {
		Name        string `json:"name"`
		Role        string `json:"role,omitempty"`
		Description string `json:"description,omitempty"`
		Status      string `json:"status"`
		IsExecutive bool   `json:"is_executive,omitempty"`
		ReportsTo   string `json:"reports_to,omitempty"`
		ProfileSlug string `json:"profile_slug,omitempty"`
		LastSeen    string `json:"last_seen"`
		Activity    string `json:"activity,omitempty"`
	}

	result := make([]agentEntry, 0, len(agents))
	for _, a := range agents {
		// Truncate description to keep the list payload bounded; full bio is on
		// the agent's profile / get_profile.
		if len(a.Description) > 200 {
			a.Description = a.Description[:200] + "…"
		}
		entry := agentEntry{
			Name:        a.Name,
			Role:        a.Role,
			Description: a.Description,
			Status:      a.Status,
			IsExecutive: a.IsExecutive,
			ReportsTo:   strOrDash(a.ReportsTo),
			ProfileSlug: strOrDash(a.ProfileSlug),
			LastSeen:    a.LastSeen,
		}
		if a.SessionID != nil {
			if s, ok := sessionByID[*a.SessionID]; ok {
				entry.Activity = string(s.Activity)
			}
		}
		result = append(result, entry)
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(result))
		for i, a := range result {
			exec := ""
			if a.IsExecutive {
				exec = "yes"
			}
			rows[i] = []string{a.Name, a.Role, a.Status, exec, a.LastSeen, a.Activity, a.Description}
		}
		table := renderTable([]string{"name", "role", "status", "executive", "last_seen", "activity", "description"}, rows)
		return h.resultTextTracked(project, "", "list_agents", fmt.Sprintf("%d agents\n%s", len(result), table))
	}

	return h.resultJSONTracked(project, "", "list_agents", map[string]any{
		"count":  len(result),
		"agents": result,
	})
}

func (h *Handlers) HandleMarkRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)

	// Support marking a whole conversation as read
	convID := req.GetString("conversation_id", "")
	if convID != "" {
		if err := h.db.MarkConversationRead(convID, agent); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to mark conversation read: %v", err)), nil
		}
		return h.resultJSONTracked(project, agent, "mark_read", map[string]any{
			"conversation_id": convID,
			"marked_read":     true,
		})
	}

	ids := req.GetStringSlice("message_ids", nil)
	// Common mistake: singular message_id. Accept it as a one-element array.
	if len(ids) == 0 {
		if single := req.GetString("message_id", ""); single != "" {
			ids = []string{single}
		}
	}
	if len(ids) == 0 {
		return mcp.NewToolResultError("message_ids (array) or conversation_id is required. Note: the field is plural — pass message_ids:['id1','id2'] not message_id."), nil
	}

	count, err := h.db.MarkRead(ids, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to mark read: %v", err)), nil
	}

	return h.resultJSONTracked(project, agent, "mark_read", map[string]any{
		"marked_read": count,
	})
}

func (h *Handlers) HandleCreateConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	title := req.GetString("title", "")
	if title == "" {
		return mcp.NewToolResultError("title is required"), nil
	}

	rawMembers := req.GetStringSlice("members", nil)
	if len(rawMembers) == 0 {
		return mcp.NewToolResultError("at least one other member is required"), nil
	}
	members := make([]string, len(rawMembers))
	for i, m := range rawMembers {
		members[i] = strings.ToLower(m)
	}

	// Ensure creator is included in members
	found := false
	for _, m := range members {
		if m == agent {
			found = true
			break
		}
	}
	if !found {
		members = append([]string{agent}, members...)
	}

	conv, err := h.db.CreateConversation(project, title, agent, members)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create conversation: %v", err)), nil
	}

	return h.resultJSONTracked(project, agent, "create_conversation", map[string]any{
		"conversation": conv,
		"members":      members,
	})
}

func (h *Handlers) HandleListConversations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)

	convs, err := h.db.ListConversations(project, agent)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list conversations: %v", err)), nil
	}
	if convs == nil {
		convs = []models.ConversationSummary{}
	}

	return h.resultJSONTracked(project, agent, "list_conversations", map[string]any{
		"agent":         agent,
		"count":         len(convs),
		"conversations": convs,
	})
}

func (h *Handlers) HandleGetConversationMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agent := resolveAgent(ctx, req)
	convID := req.GetString("conversation_id", "")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}
	limit := req.GetInt("limit", 50)

	// Verify membership
	isMember, err := h.db.IsConversationMember(convID, agent)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to check membership: %v", err)), nil
	}
	if !isMember {
		return mcp.NewToolResultError("you are not a member of this conversation"), nil
	}

	messages, err := h.db.GetConversationMessages(convID, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get messages: %v", err)), nil
	}
	if messages == nil {
		messages = []models.Message{}
	}

	// Auto-mark conversation as read when fetching messages
	_ = h.db.MarkConversationRead(convID, agent)

	format := req.GetString("format", "full")

	formatted := make([]map[string]any, len(messages))
	for i, m := range messages {
		entry := map[string]any{
			"id":         m.ID,
			"from":       m.From,
			"type":       m.Type,
			"subject":    m.Subject,
			"created_at": m.CreatedAt,
		}
		if m.ReplyTo != nil {
			entry["reply_to"] = *m.ReplyTo
		}
		switch format {
		case "compact":
			// metadata only — no content
		case "digest":
			c := m.Content
			if len(c) > 200 {
				c = c[:200] + "..."
			}
			entry["content"] = c
		default: // "full"
			entry["content"] = m.Content
			if m.Metadata != "" && m.Metadata != "{}" {
				entry["metadata"] = m.Metadata
			}
		}
		formatted[i] = entry
	}

	return h.resultJSONTracked(resolveProject(ctx, req), agent, "get_conversation_messages", map[string]any{
		"conversation_id": convID,
		"count":           len(formatted),
		"format":          format,
		"messages":        formatted,
	})
}

func (h *Handlers) HandleInviteToConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	convID := req.GetString("conversation_id", "")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}
	invitee := strings.ToLower(req.GetString("agent_name", ""))
	if invitee == "" {
		return mcp.NewToolResultError("agent_name is required"), nil
	}

	// Verify inviter is a member
	isMember, err := h.db.IsConversationMember(convID, agent)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to check membership: %v", err)), nil
	}
	if !isMember {
		return mcp.NewToolResultError("you are not a member of this conversation"), nil
	}

	if err := h.db.AddConversationMember(convID, invitee); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to invite: %v", err)), nil
	}

	// Notify the invitee
	h.registry.Notify(project, invitee, agent, fmt.Sprintf("You were invited to conversation: %s", convID), "")

	return h.resultJSONTracked(project, agent, "invite_to_conversation", map[string]any{
		"conversation_id": convID,
		"invited":         invitee,
	})
}

func (h *Handlers) HandleLeaveConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	convID := req.GetString("conversation_id", "")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}

	isMember, err := h.db.IsConversationMember(convID, agent)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to check membership: %v", err)), nil
	}
	if !isMember {
		return mcp.NewToolResultError("you are not a member of this conversation"), nil
	}

	if err := h.db.LeaveConversation(convID, agent); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to leave: %v", err)), nil
	}

	h.events.Emit(MCPEvent{
		Type:    "conversation",
		Action:  "left",
		Agent:   agent,
		Project: project,
		Label:   convID,
	})

	return h.resultJSONTracked(project, agent, "leave_conversation", map[string]any{
		"conversation_id": convID,
		"left":            agent,
	})
}

func (h *Handlers) HandleArchiveConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	convID := req.GetString("conversation_id", "")
	if convID == "" {
		return mcp.NewToolResultError("conversation_id is required"), nil
	}

	if err := h.db.ArchiveConversation(convID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to archive: %v", err)), nil
	}

	h.events.Emit(MCPEvent{
		Type:    "conversation",
		Action:  "archived",
		Agent:   resolveAgent(ctx, req),
		Project: project,
		Label:   convID,
	})

	return h.resultJSONTracked(project, "", "archive_conversation", map[string]any{
		"conversation_id": convID,
		"archived":        true,
	})
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

func (h *Handlers) HandleSetMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	key := req.GetString("key", "")
	if key == "" {
		return mcp.NewToolResultError("key is required"), nil
	}
	value := req.GetString("value", "")
	if value == "" {
		return mcp.NewToolResultError("value is required"), nil
	}
	scope := req.GetString("scope", "project")
	confidence := req.GetString("confidence", "stated")
	layer := req.GetString("layer", "behavior")
	tags := req.GetStringSlice("tags", nil)
	tagsJSON := db.TagsToJSON(tags)
	upsert := req.GetBool("upsert", true)

	mem, err := h.db.SetMemory(project, agent, key, value, tagsJSON, scope, confidence, layer, upsert)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to set memory: %v", err)), nil
	}

	result := map[string]any{
		"memory": mem,
	}
	action := "set"
	if mem.ConflictWith != nil {
		result["conflict"] = true
		result["message"] = fmt.Sprintf("Conflict detected: key '%s' already exists with a different value. Both versions preserved. Use resolve_conflict to pick the truth.", key)
		action = "conflict"
	}
	h.events.Emit(MCPEvent{Type: "memory", Action: action, Agent: agent, Project: project, Label: key})

	return h.resultJSONTracked(project, agent, "set_memory", result)
}

func (h *Handlers) HandleGetMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	key := req.GetString("key", "")
	if key == "" {
		return mcp.NewToolResultError("key is required"), nil
	}
	scope := req.GetString("scope", "")

	memories, err := h.db.GetMemory(project, agent, key, scope)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get memory: %v", err)), nil
	}
	if memories == nil {
		memories = []models.Memory{}
	}

	result := map[string]any{
		"key":      key,
		"count":    len(memories),
		"memories": memories,
	}
	if len(memories) > 1 {
		result["conflict"] = true
		result["message"] = "Multiple values exist for this key. Use resolve_conflict to pick the truth."
	}

	return h.resultJSONTracked(project, agent, "get_memory", result)
}

func (h *Handlers) HandleSearchMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	scope := req.GetString("scope", "")
	tags := req.GetStringSlice("tags", nil)
	limit := req.GetInt("limit", 20)

	memories, err := h.db.SearchMemory(project, agent, query, tags, scope, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to search memories: %v", err)), nil
	}
	if memories == nil {
		memories = []models.Memory{}
	}

	// Truncate values for compact response
	truncated := make([]map[string]any, len(memories))
	for i, m := range memories {
		val := m.Value
		if len(val) > 300 {
			val = val[:300] + "..."
		}
		truncated[i] = map[string]any{
			"id":         m.ID,
			"key":        m.Key,
			"value":      val,
			"tags":       m.Tags,
			"scope":      m.Scope,
			"agent_name": m.AgentName,
			"confidence": m.Confidence,
			"version":    m.Version,
			"updated_at": m.UpdatedAt,
			"conflict":   m.ConflictWith != nil,
		}
	}

	return h.resultJSONTracked(project, agent, "search_memory", map[string]any{
		"query":    query,
		"count":    len(truncated),
		"memories": truncated,
	})
}

func (h *Handlers) HandleListMemories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	scope := req.GetString("scope", "")
	agentFilter := req.GetString("agent", "")
	tags := req.GetStringSlice("tags", nil)
	limit := req.GetInt("limit", 50)

	// Bug fix: scope=agent must be filtered by the calling agent to prevent leaking
	// other agents' private memories. If no explicit agent filter, use the caller's identity.
	if scope == "agent" && agentFilter == "" {
		agentFilter = resolveAgent(ctx, req)
	}

	memories, err := h.db.ListMemories(project, scope, agentFilter, tags, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list memories: %v", err)), nil
	}
	if memories == nil {
		memories = []models.Memory{}
	}

	// Truncate values for compact response
	truncated := make([]map[string]any, len(memories))
	for i, m := range memories {
		val := m.Value
		if len(val) > 200 {
			val = val[:200] + "..."
		}
		truncated[i] = map[string]any{
			"id":         m.ID,
			"key":        m.Key,
			"value":      val,
			"tags":       m.Tags,
			"scope":      m.Scope,
			"project":    m.Project,
			"agent_name": m.AgentName,
			"confidence": m.Confidence,
			"version":    m.Version,
			"updated_at": m.UpdatedAt,
			"conflict":   m.ConflictWith != nil,
		}
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(memories))
		for i, m := range memories {
			val, _ := truncated[i]["value"].(string)
			rows[i] = []string{m.Key, m.Scope, m.AgentName, m.Confidence, m.Tags, val, m.UpdatedAt}
		}
		table := renderTable([]string{"key", "scope", "agent", "confidence", "tags", "value", "updated_at"}, rows)
		return h.resultTextTracked(project, "", "list_memories", fmt.Sprintf("%d memories\n%s", len(memories), table))
	}

	return h.resultJSONTracked(project, "", "list_memories", map[string]any{
		"count":    len(truncated),
		"memories": truncated,
	})
}

func (h *Handlers) HandleDeleteMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	key := req.GetString("key", "")
	if key == "" {
		return mcp.NewToolResultError("key is required"), nil
	}
	scope := req.GetString("scope", "project")

	if err := h.db.DeleteMemory(project, agent, key, scope); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to delete memory: %v", err)), nil
	}

	return h.resultJSONTracked(project, agent, "delete_memory", map[string]any{
		"deleted": true,
		"key":     key,
		"scope":   scope,
	})
}

func (h *Handlers) HandleResolveConflict(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	key := req.GetString("key", "")
	if key == "" {
		return mcp.NewToolResultError("key is required"), nil
	}
	chosenValue := req.GetString("chosen_value", "")
	if chosenValue == "" {
		return mcp.NewToolResultError("chosen_value is required"), nil
	}
	scope := req.GetString("scope", "project")

	winner, err := h.db.ResolveConflict(project, agent, key, chosenValue, scope)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to resolve conflict: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "memory", Action: "resolve", Agent: agent, Project: project, Label: key})

	return h.resultJSONTracked(project, agent, "resolve_conflict", map[string]any{
		"resolved": true,
		"memory":   winner,
	})
}

// --- Profile handlers ---

func (h *Handlers) HandleRegisterProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	slug := req.GetString("slug", "")
	if slug == "" {
		return mcp.NewToolResultError("slug is required"), nil
	}
	name := req.GetString("name", "")
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	role := req.GetString("role", "")
	skills := normalizeJSONArrayParam(req, "skills")

	profile, err := h.db.RegisterProfile(project, slug, name, role, skills)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to register profile: %v", err)), nil
	}
	return h.resultJSONTracked(project, "", "register_profile", profile)
}

func (h *Handlers) HandleGetProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	slug := req.GetString("slug", "")
	if slug == "" {
		return mcp.NewToolResultError("slug is required"), nil
	}

	profile, err := h.db.GetProfile(project, slug)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get profile: %v", err)), nil
	}
	if profile == nil {
		return mcp.NewToolResultError(fmt.Sprintf("profile not found: %s", slug)), nil
	}
	return h.resultJSONTracked(project, "", "get_profile", profile)
}

func (h *Handlers) HandleListProfiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)

	profiles, err := h.db.ListProfiles(project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list profiles: %v", err)), nil
	}
	if profiles == nil {
		profiles = []models.Profile{}
	}

	return h.resultJSONTracked(project, "", "list_profiles", map[string]any{
		"count":    len(profiles),
		"profiles": profiles,
	})
}

// --- Task handlers ---

func (h *Handlers) HandleDispatchTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	profile := req.GetString("profile", "")
	requiredSkill := req.GetString("required_skill", "")
	// Quota check: tasks
	if qErr := h.db.CheckQuotaError(project, agent, "tasks"); qErr != "" {
		return mcp.NewToolResultError(qErr), nil
	}

	// Auto-resolve profile from skill if not specified
	if profile == "" && requiredSkill != "" {
		best, _ := h.db.FindBestProfileForSkill(project, requiredSkill)
		if best != nil {
			profile = best.Slug
		}
	}
	if profile == "" {
		return mcp.NewToolResultError("profile is required (or provide required_skill)"), nil
	}
	title := req.GetString("title", "")
	if title == "" {
		return mcp.NewToolResultError("title is required"), nil
	}
	description := req.GetString("description", "")
	priority := req.GetString("priority", "P2")
	parentTaskID := optionalString(req.GetString("parent_task_id", ""))
	boardID := optionalString(req.GetString("board_id", ""))

	// Resolve truncated board_id prefix to full UUID
	if boardID != nil && len(*boardID) < 36 {
		boards, _ := h.db.ListBoards(project)
		for _, b := range boards {
			if strings.HasPrefix(b.ID, *boardID) {
				boardID = &b.ID
				break
			}
		}
	}

	// Auto-create "human" profile if dispatching to it for the first time
	if profile == "human" {
		existing, _ := h.db.GetProfile(project, "human")
		if existing == nil {
			_, _ = h.db.RegisterProfile(project, "human", "Human Operator",
				"Tasks that require human action (API keys, approvals, purchases, manual config)",
				"[]")
		}
	}

	// Auto-create a default "backlog" board if none specified and none exist
	var autoBoard *models.Board
	if boardID == nil {
		boards, _ := h.db.ListBoards(project)
		if len(boards) == 0 {
			autoBoard, _ = h.db.CreateBoard(project, "Backlog", "backlog", "Auto-created default board", agent)
			if autoBoard != nil {
				boardID = &autoBoard.ID
			}
		} else {
			// Use the first existing board as default
			boardID = &boards[0].ID
		}
	}

	task, err := h.db.DispatchTask(project, profile, agent, title, description, priority, parentTaskID, boardID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to dispatch task: %v", err)), nil
	}

	// Push notification for P0/P1 tasks
	if priority == "P0" || priority == "P1" {
		h.registry.NotifyProfile(project, profile, agent, fmt.Sprintf("[%s] %s", priority, title), task.ID)
	}

	// Auto-notification: send inbox message to agents running this profile
	agents, _ := h.db.GetAgentsByProfile(project, profile)
	for _, a := range agents {
		if a.Name == agent {
			continue // don't notify the dispatcher
		}
		subject := fmt.Sprintf("New task: %s", title)
		content := fmt.Sprintf("[%s] %s\n\nTask ID: %s\nProfile: %s\nDispatched by: %s", priority, title, task.ID, profile, agent)
		if description != "" && len(description) <= 200 {
			content += "\n\n" + description
		}
		taskID := task.ID
		_, _ = h.db.InsertMessage(project, agent, a.Name, "task", subject, content, fmt.Sprintf(`{"task_id":"%s"}`, taskID), "P2", 14400, nil, nil)
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "dispatch", Agent: agent, Project: project, Target: profile, Label: title})
	emitTaskEvent(h.events, "task.dispatched", "dispatch", project, task)

	resp := map[string]any{"task": task}
	if autoBoard != nil {
		resp["auto_board"] = autoBoard
		resp["hint"] = fmt.Sprintf("Auto-created 'backlog' board (id: %s) since no boards existed.", autoBoard.ID)
	}

	// Dedup warning: check for similar active tasks on same profile
	similar, _ := h.db.FindSimilarTasks(project, profile, title)
	if len(similar) > 0 {
		// Filter out the task we just created
		var dupes []map[string]string
		for _, s := range similar {
			if s.ID != task.ID {
				dupes = append(dupes, map[string]string{"id": s.ID, "title": s.Title, "status": s.Status})
			}
		}
		if len(dupes) > 0 {
			resp["warning"] = fmt.Sprintf("Found %d similar active task(s) on profile '%s'", len(dupes), profile)
			resp["similar"] = dupes
		}
	}

	return h.resultJSONTracked(project, agent, "dispatch_task", resp)
}

func (h *Handlers) HandleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	task, err := h.db.ClaimTask(taskID, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to claim task: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "claim", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.claimed", "claim", project, task)
	return h.resultJSONTracked(project, agent, "claim_task", task)
}

func (h *Handlers) HandleStartTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	task, err := h.db.StartTask(taskID, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to start task: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "start", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.in_progress", "start", project, task)
	return h.resultJSONTracked(project, agent, "start_task", task)
}

// HandleResumeTask transitions a blocked task back to in-progress.
// Thin wrapper over StartTask (the DB allows the blocked→in-progress transition
// already) — kept as a distinct MCP tool so agents discovering tools don't have
// to guess that start_task resumes too.
func (h *Handlers) HandleResumeTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	existing, err := h.db.GetTask(taskID, project)
	if err != nil || existing == nil {
		return mcp.NewToolResultError("task not found"), nil
	}
	if existing.Status != "blocked" {
		return mcp.NewToolResultError(fmt.Sprintf("task is not blocked (status=%s)", existing.Status)), nil
	}

	task, err := h.db.StartTask(taskID, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to resume task: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "resume", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.in_progress", "resume", project, task)

	return h.resultJSONTracked(project, agent, "resume_task", task)
}

func (h *Handlers) HandleReviewTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	task, err := h.db.ReviewTask(taskID, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to mark task in-review: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "review", Agent: agent, Project: project, Target: task.DispatchedBy, Label: task.Title})
	emitTaskEvent(h.events, "task.in_review", "review", project, task)

	// Notify dispatcher — work is up for review.
	h.registry.Notify(project, task.DispatchedBy, agent, fmt.Sprintf("In review: %s", task.Title), task.ID)

	// Write-back (Linear mode): after the local stamp succeeds, fire-and-forget
	// the agent's one owned transition (→ In Review + comment). No-op in native.
	comment := optionalString(req.GetString("comment", ""))
	pushInReviewAsync(h.connector, task, agent, comment)

	return h.resultJSONTracked(project, agent, "review_task", task)
}

func (h *Handlers) HandleCompleteTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result := optionalString(req.GetString("result", ""))

	task, err := h.db.CompleteTask(taskID, agent, project, result)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to complete task: %v", err)), nil
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "complete", Agent: agent, Project: project, Target: task.DispatchedBy, Label: task.Title})
	emitTaskEvent(h.events, "task.done", "complete", project, task)

	// Notify dispatcher
	h.registry.Notify(project, task.DispatchedBy, agent, fmt.Sprintf("Task done: %s", task.Title), task.ID)

	// If this task has a parent, check if all sibling subtasks are now complete
	if task.ParentTaskID != nil {
		allDone, total, doneCount := h.db.CheckSubtasksComplete(*task.ParentTaskID, project)
		if allDone {
			parent, _ := h.db.GetTask(*task.ParentTaskID, project)
			if parent != nil {
				h.registry.Notify(project, parent.DispatchedBy, agent,
					fmt.Sprintf("All %d subtasks complete for: %s", total, parent.Title), parent.ID)
				// Also notify the assigned agent on the parent task
				if parent.AssignedTo != nil && *parent.AssignedTo != parent.DispatchedBy {
					h.registry.Notify(project, *parent.AssignedTo, agent,
						fmt.Sprintf("All %d subtasks complete for your task: %s", total, parent.Title), parent.ID)
				}
			}
		} else {
			// Partial progress notification to parent dispatcher
			parent, _ := h.db.GetTask(*task.ParentTaskID, project)
			if parent != nil {
				h.registry.Notify(project, parent.DispatchedBy, agent,
					fmt.Sprintf("Subtask done (%d/%d): %s → %s", doneCount, total, task.Title, parent.Title), parent.ID)
			}
		}
	}

	return h.resultJSONTracked(project, agent, "complete_task", task)
}

func (h *Handlers) HandleBlockTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reason := optionalString(req.GetString("reason", ""))

	task, err := h.db.BlockTask(taskID, agent, project, reason)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to block task: %v", err)), nil
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "block", Agent: agent, Project: project, Target: task.DispatchedBy, Label: task.Title})
	blockedExtra := map[string]any{}
	if reason != nil {
		blockedExtra["reason"] = *reason
	}
	emitTaskEvent(h.events, "task.blocked", "block", project, task, blockedExtra)

	// Notify dispatcher — blocked is critical
	reasonStr := ""
	if reason != nil {
		reasonStr = ": " + *reason
	}
	h.registry.Notify(project, task.DispatchedBy, agent, fmt.Sprintf("BLOCKED: %s%s", task.Title, reasonStr), task.ID)

	// Phase 4: Bubble notification up parent chain
	if task.ParentTaskID != nil {
		parentChain, _ := h.db.GetParentChain(taskID, project)
		for _, parent := range parentChain {
			h.registry.Notify(project, parent.DispatchedBy, agent,
				fmt.Sprintf("Subtask blocked: '%s' → %s%s", task.Title, parent.Title, reasonStr), task.ID)
		}
	}

	return h.resultJSONTracked(project, agent, "block_task", task)
}

func (h *Handlers) HandleCancelTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reason := optionalString(req.GetString("reason", ""))

	task, err := h.db.CancelTask(taskID, agent, project, reason)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to cancel task: %v", err)), nil
	}

	// Notify dispatcher
	reasonStr := ""
	if reason != nil {
		reasonStr = ": " + *reason
	}
	h.registry.Notify(project, task.DispatchedBy, agent, fmt.Sprintf("Task cancelled: %s%s", task.Title, reasonStr), task.ID)

	// Notify assigned agent (if different from canceller and dispatcher)
	if task.AssignedTo != nil && *task.AssignedTo != agent && *task.AssignedTo != task.DispatchedBy {
		h.registry.Notify(project, *task.AssignedTo, agent, fmt.Sprintf("Your task was cancelled: %s%s", task.Title, reasonStr), task.ID)
	}

	// If this task has a parent, check if all sibling subtasks are now complete (cancelled counts)
	if task.ParentTaskID != nil {
		allDone, total, doneCount := h.db.CheckSubtasksComplete(*task.ParentTaskID, project)
		if allDone {
			parent, _ := h.db.GetTask(*task.ParentTaskID, project)
			if parent != nil {
				h.registry.Notify(project, parent.DispatchedBy, agent,
					fmt.Sprintf("All %d subtasks resolved for: %s", total, parent.Title), parent.ID)
			}
		} else {
			parent, _ := h.db.GetTask(*task.ParentTaskID, project)
			if parent != nil {
				h.registry.Notify(project, parent.DispatchedBy, agent,
					fmt.Sprintf("Subtask cancelled (%d/%d resolved): %s → %s", doneCount, total, task.Title, parent.Title), parent.ID)
			}
		}
	}

	return h.resultJSONTracked(project, agent, "cancel_task", task)
}

func (h *Handlers) HandleUpdateTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	title := optionalString(req.GetString("title", ""))
	description := optionalString(req.GetString("description", ""))
	priority := optionalString(req.GetString("priority", ""))
	boardID := optionalString(req.GetString("board_id", ""))
	progressNote := req.GetString("progress_note", "")

	task, err := h.db.UpdateTaskFields(taskID, project, title, description, priority, boardID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to update task: %v", err)), nil
	}

	if progressNote != "" {
		if err := h.db.AddProgressNote(taskID, project, agent, progressNote); err == nil {
			h.events.Emit(MCPEvent{Type: "task", Action: "progress", Agent: agent, Project: project, Label: task.Title})
		}
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "update", Agent: agent, Project: project, Label: task.Title})
	return h.resultJSONTracked(project, agent, "update_task", task)
}

func (h *Handlers) HandleArchiveTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	status := req.GetString("status", "")
	boardID := req.GetString("board_id", "")

	count, err := h.db.ArchiveTasks(project, status, boardID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to archive tasks: %v", err)), nil
	}

	msg := fmt.Sprintf("Archived %d tasks", count)
	if status != "" {
		msg += fmt.Sprintf(" (status=%s)", status)
	}
	if boardID != "" {
		msg += fmt.Sprintf(" (board=%s)", boardID)
	}
	return mcp.NewToolResultText(msg), nil
}

func (h *Handlers) HandleMoveTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	boardID := optionalString(req.GetString("board_id", ""))

	if boardID == nil {
		return mcp.NewToolResultError("board_id is required"), nil
	}

	// Resolve truncated board_id prefix
	if len(*boardID) > 0 && len(*boardID) < 36 {
		boards, _ := h.db.ListBoards(project)
		for _, b := range boards {
			if strings.HasPrefix(b.ID, *boardID) {
				boardID = &b.ID
				break
			}
		}
	}

	task, err := h.db.UpdateTaskFields(taskID, project, nil, nil, nil, boardID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to move task: %v", err)), nil
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "move", Agent: agent, Project: project, Label: task.Title})
	return h.resultJSONTracked(project, agent, "move_task", task)
}

func (h *Handlers) HandleBatchCompleteTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	tasksJSON := req.GetString("tasks", "")

	var items []struct {
		TaskID string  `json:"task_id"`
		Result *string `json:"result"`
	}
	// Accept the common mistake task_ids:["..."] as a shorthand for
	// tasks:[{task_id:"..."}] (no result).
	if tasksJSON == "" {
		if idsJSON := req.GetString("task_ids", ""); idsJSON != "" {
			var ids []string
			if err := json.Unmarshal([]byte(idsJSON), &ids); err == nil {
				for _, id := range ids {
					items = append(items, struct {
						TaskID string  `json:"task_id"`
						Result *string `json:"result"`
					}{TaskID: id})
				}
			}
		}
	} else {
		if err := json.Unmarshal([]byte(tasksJSON), &items); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid tasks JSON: %v", err)), nil
		}
	}
	if len(items) == 0 {
		return mcp.NewToolResultError("tasks is required — pass tasks:'[{\"task_id\":\"...\",\"result\":\"...\"}]' (JSON string). As a shortcut, task_ids:'[\"id1\",\"id2\"]' is also accepted."), nil
	}

	var completed []string
	var errors []string
	for _, item := range items {
		taskID, err := h.resolveTaskID(item.TaskID, project)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", item.TaskID, err))
			continue
		}
		task, err := h.db.CompleteTask(taskID, agent, project, item.Result)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", taskID, err))
			continue
		}
		completed = append(completed, taskID)
		h.events.Emit(MCPEvent{Type: "task", Action: "complete", Agent: agent, Project: project, Label: task.Title})
	}

	return h.resultJSONTracked(project, agent, "batch_complete_tasks", map[string]any{
		"completed": completed,
		"errors":    errors,
		"total":     len(items),
	})
}

func (h *Handlers) HandleBatchDispatchTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	tasksJSON := req.GetString("tasks", "[]")

	var items []struct {
		Profile     string  `json:"profile"`
		Title       string  `json:"title"`
		Description string  `json:"description"`
		Priority    string  `json:"priority"`
		BoardID     *string `json:"board_id"`
	}
	if err := json.Unmarshal([]byte(tasksJSON), &items); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid tasks JSON: %v", err)), nil
	}
	if len(items) == 0 {
		return mcp.NewToolResultError("tasks is required — pass tasks:'[{\"profile\":\"...\",\"title\":\"...\",\"priority\":\"P2\",\"board_id\":\"...\"}]' (JSON string). Only profile and title are required per item."), nil
	}

	var dispatched []map[string]string
	var errors []string
	for _, item := range items {
		if item.Profile == "" || item.Title == "" {
			errors = append(errors, fmt.Sprintf("missing profile or title: %+v", item))
			continue
		}
		priority := item.Priority
		if priority == "" {
			priority = "P2"
		}
		task, err := h.db.DispatchTask(project, item.Profile, agent, item.Title, item.Description, priority, nil, item.BoardID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", item.Title, err))
			continue
		}
		dispatched = append(dispatched, map[string]string{"id": task.ID, "title": task.Title})
		h.events.Emit(MCPEvent{Type: "task", Action: "dispatch", Agent: agent, Project: project, Target: item.Profile, Label: item.Title})
		emitTaskEvent(h.events, "task.dispatched", "dispatch", project, task)
	}

	return h.resultJSONTracked(project, agent, "batch_dispatch_tasks", map[string]any{
		"dispatched": dispatched,
		"errors":     errors,
		"total":      len(items),
	})
}

func (h *Handlers) HandleGetTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, rErr := h.resolveTaskID(taskID, project)
	if rErr != nil {
		return mcp.NewToolResultError(rErr.Error()), nil
	}
	includeSubtasks := req.GetBool("include_subtasks", false)

	var task *models.Task
	var err error
	if includeSubtasks {
		task, err = h.db.GetTaskWithSubtasks(taskID, project)
	} else {
		task, err = h.db.GetTask(taskID, project)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get task: %v", err)), nil
	}
	if task == nil {
		return mcp.NewToolResultError("task not found"), nil
	}

	return h.resultJSONTracked(project, "", "get_task", task)
}

func (h *Handlers) HandleListTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	status := req.GetString("status", "")
	profile := req.GetString("profile", "")
	priority := req.GetString("priority", "")
	assignedTo := req.GetString("assigned_to", "")
	boardID := req.GetString("board_id", "")
	limit := req.GetInt("limit", 50)
	includeArchived := req.GetBool("include_archived", false)

	tasks, err := h.db.ListTasks(project, status, profile, priority, assignedTo, boardID, limit, includeArchived)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list tasks: %v", err)), nil
	}
	if tasks == nil {
		tasks = []models.Task{}
	}

	// Truncate descriptions to save tokens in list view (use get_task for full details)
	for i := range tasks {
		if len(tasks[i].Description) > 200 {
			tasks[i].Description = tasks[i].Description[:200] + "…"
		}
		if tasks[i].Result != nil && len(*tasks[i].Result) > 200 {
			truncated := (*tasks[i].Result)[:200] + "…"
			tasks[i].Result = &truncated
		}
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(tasks))
		for i, t := range tasks {
			outcome := strOrDash(t.Result)
			if t.Status == "blocked" {
				outcome = "BLOCKED: " + strOrDash(t.BlockedReason)
			}
			rows[i] = []string{
				t.ID, t.Status, t.Priority, t.ProfileSlug, strOrDash(t.AssignedTo),
				t.Title, t.Description, outcome,
			}
		}
		table := renderTable([]string{"id", "status", "priority", "profile", "assigned_to", "title", "description", "result_or_blocked_reason"}, rows)
		return h.resultTextTracked(project, "", "list_tasks", fmt.Sprintf("%d tasks\n%s", len(tasks), table))
	}

	return h.resultJSONTracked(project, "", "list_tasks", map[string]any{
		"count": len(tasks),
		"tasks": tasks,
	})
}

// --- File locks ---

func (h *Handlers) HandleClaimFiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	filePaths := req.GetString("file_paths", "[]")
	ttlSeconds := req.GetInt("ttl_seconds", 1800)

	// Surface existing claims on the same files (advisory: no hard refusal,
	// but the caller learns who else is editing so they can coordinate).
	var requested []string
	_ = json.Unmarshal([]byte(filePaths), &requested)
	existing := h.findExistingClaims(project, agent, requested)

	lock, err := h.db.ClaimFiles(project, agent, filePaths, ttlSeconds)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to claim files: %v", err)), nil
	}

	// Auto-broadcast steering message
	subject := fmt.Sprintf("%s claimed files", agent)
	content := fmt.Sprintf("%s is now editing: %s", agent, filePaths)
	_, _ = h.db.InsertMessage(project, agent, "*", "notification", subject, content, fmt.Sprintf(`{"tags":["file-lock"],"file_paths":%s}`, filePaths), "P1", 0, nil, nil)

	payload := map[string]any{"lock": lock}
	if len(existing) > 0 {
		payload["existing_claims"] = existing
		payload["conflict"] = true
	}
	return h.resultJSONTracked(project, agent, "claim_files", payload)
}

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

func (h *Handlers) HandleReleaseFiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	filePaths := req.GetString("file_paths", "[]")

	if err := h.db.ReleaseFiles(project, agent, filePaths); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to release files: %v", err)), nil
	}

	// Auto-broadcast info message
	subject := fmt.Sprintf("%s released files", agent)
	content := fmt.Sprintf("%s released: %s", agent, filePaths)
	_, _ = h.db.InsertMessage(project, agent, "*", "notification", subject, content, fmt.Sprintf(`{"tags":["file-lock"],"file_paths":%s}`, filePaths), "P3", 3600, nil, nil)

	return h.resultJSONTracked(project, agent, "release_files", map[string]any{"released": filePaths})
}

func (h *Handlers) HandleListLocks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)

	locks, err := h.db.ListFileLocks(project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list locks: %v", err)), nil
	}
	if locks == nil {
		locks = []models.FileLock{}
	}

	return h.resultJSONTracked(project, "", "list_locks", map[string]any{
		"count": len(locks),
		"locks": locks,
	})
}

// --- Agent lifecycle ---

func (h *Handlers) HandleDeactivateAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	name := strings.ToLower(req.GetString("name", ""))
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}

	if err := h.db.DeactivateAgent(project, name); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to deactivate agent: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "register", Action: "deactivate", Agent: name, Project: project})

	return h.resultJSONTracked(project, name, "deactivate_agent", map[string]any{
		"deactivated": true,
		"agent":       name,
	})
}

func (h *Handlers) HandleCreateBoard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	name := req.GetString("name", "")
	slug := req.GetString("slug", "")
	if name == "" || slug == "" {
		return mcp.NewToolResultError("name and slug are required"), nil
	}
	description := req.GetString("description", "")

	board, err := h.db.CreateBoard(project, name, slug, description, agent)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create board: %v", err)), nil
	}
	return h.resultJSONTracked(project, agent, "create_board", board)
}

func (h *Handlers) HandleListBoards(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	boards, err := h.db.ListBoards(project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list boards: %v", err)), nil
	}
	if boards == nil {
		boards = []models.Board{}
	}
	return h.resultJSONTracked(project, "", "list_boards", boards)
}

func (h *Handlers) HandleArchiveBoard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	boardID := req.GetString("board_id", "")
	if boardID == "" {
		return mcp.NewToolResultError("board_id is required"), nil
	}

	if err := h.db.ArchiveBoard(project, boardID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to archive board: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Board %s archived (with all its tasks)", boardID)), nil
}

func (h *Handlers) HandleDeleteBoard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	boardID := req.GetString("board_id", "")
	if boardID == "" {
		return mcp.NewToolResultError("board_id is required"), nil
	}

	if err := h.db.DeleteBoard(project, boardID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to delete board: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Board %s permanently deleted", boardID)), nil
}

func (h *Handlers) HandleDeleteAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	name := strings.ToLower(req.GetString("name", ""))
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}

	if err := h.db.DeleteAgent(project, name); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to delete agent: %v", err)), nil
	}

	return h.resultJSONTracked(project, name, "delete_agent", map[string]any{
		"deleted": true,
		"agent":   name,
	})
}

func (h *Handlers) HandleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := strings.ToLower(req.GetString("name", ""))
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	description := req.GetString("description", "")
	cwd := req.GetString("cwd", "")

	// Create project in DB
	h.db.EnsureProject(name)

	// Check if already configured
	agents, _ := h.db.ListAgents(name)
	if len(agents) > 0 {
		return h.resultJSONTracked(resolveProject(ctx, req), name, "create_project", map[string]any{
			"project": name,
			"status":  "already_configured",
			"agents":  len(agents),
			"hint":    "Project already has agents. Use register_agent to join, or delete_project to start over.",
		})
	}

	interactive := false
	if v, ok := req.GetArguments()["interactive"]; ok {
		if b, ok := v.(bool); ok {
			interactive = b
		}
	}

	// Return the onboarding mega-prompt as plain text
	prompt := buildOnboardingPrompt(name, description, cwd, interactive)
	return mcp.NewToolResultText(prompt), nil
}

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

func (h *Handlers) HandleDeleteProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := req.GetString("project", "")
	if project == "" {
		return mcp.NewToolResultError("project is required"), nil
	}

	if err := h.db.DeleteProject(project); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to delete project: %v", err)), nil
	}

	return h.resultJSONTracked(resolveProject(ctx, req), "", "delete_project", map[string]any{
		"deleted": true,
		"project": project,
	})
}

func (h *Handlers) HandleSleepAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)

	if err := h.db.SleepAgent(project, agent); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to sleep agent: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "register", Action: "sleep", Agent: agent, Project: project})

	return h.resultJSONTracked(project, agent, "sleep_agent", map[string]any{
		"status": "sleeping",
		"agent":  agent,
	})
}

// --- Find profiles by skill ---

func (h *Handlers) HandleFindProfiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	tag := req.GetString("skill_tag", "")
	skillName := req.GetString("skill_name", "")

	if tag == "" && skillName == "" {
		return mcp.NewToolResultError("skill_tag or skill_name is required"), nil
	}

	var profiles []models.Profile
	var err error
	searchKey := tag

	// Prefer structured skill_name (JOIN) over LIKE-based skill_tag
	if skillName != "" {
		profiles, err = h.db.FindProfilesBySkill(project, skillName)
		searchKey = skillName
	} else {
		profiles, err = h.db.FindProfilesBySkillTag(project, tag)
	}

	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to find profiles: %v", err)), nil
	}
	if profiles == nil {
		profiles = []models.Profile{}
	}

	return h.resultJSONTracked(project, "", "find_profiles", map[string]any{
		"skill":    searchKey,
		"count":    len(profiles),
		"profiles": profiles,
	})
}

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

	// Relevant memories (agent-scope + project-scope)
	memories, err := h.db.ListMemories(project, "", agentName, nil, 20)
	if err != nil || memories == nil {
		memories = []models.Memory{}
	}
	result["relevant_memories"] = memories

	// Vault/doc context is served externally (ctx.prod.synergix.ch), not injected here.

	return result
}

func (h *Handlers) HandleGetSessionContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	profileSlugParam := optionalString(req.GetString("profile_slug", ""))

	_ = h.db.TouchAgent(project, agent)

	// Auto-detect profile from agent if not provided
	if profileSlugParam == nil {
		a, err := h.db.GetAgent(project, agent)
		if err == nil && a != nil && a.ProfileSlug != nil {
			profileSlugParam = a.ProfileSlug
		}
	}

	sessionCtx := h.buildSessionContext(project, agent, profileSlugParam)
	sessionCtx["agent"] = agent
	sessionCtx["project"] = project

	return h.resultJSONTracked(project, agent, "get_session_context", sessionCtx)
}

// --- Soul RAG ---

func (h *Handlers) HandleQueryContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := req.GetInt("limit", 10)

	// Source 1: memories via FTS5
	memories, err := h.db.SearchMemory(project, agent, query, nil, "", limit)
	if err != nil {
		memories = []models.Memory{}
	}

	// Truncate memory values
	memResults := make([]map[string]any, len(memories))
	for i, m := range memories {
		val := m.Value
		if len(val) > 500 {
			val = val[:500] + "..."
		}
		memResults[i] = map[string]any{
			"type":       "memory",
			"key":        m.Key,
			"value":      val,
			"scope":      m.Scope,
			"agent_name": m.AgentName,
			"confidence": m.Confidence,
			"updated_at": m.UpdatedAt,
		}
	}

	// Source 2: completed tasks (implicit knowledge)
	doneTasks, err := h.db.ListTasks(project, "done", "", "", "", "", limit, false)
	if err != nil {
		doneTasks = []models.Task{}
	}

	// Filter tasks by relevance (simple keyword matching on title+description+result)
	taskResults := make([]map[string]any, 0)
	queryLower := strings.ToLower(query)
	for _, t := range doneTasks {
		searchable := strings.ToLower(t.Title + " " + t.Description)
		if t.Result != nil {
			searchable += " " + strings.ToLower(*t.Result)
		}
		// Simple relevance: check if any query word appears
		words := strings.Fields(queryLower)
		match := false
		for _, w := range words {
			if strings.Contains(searchable, w) {
				match = true
				break
			}
		}
		if match {
			entry := map[string]any{
				"type":         "task_result",
				"task_id":      t.ID,
				"title":        t.Title,
				"profile":      t.ProfileSlug,
				"completed_at": t.CompletedAt,
			}
			if t.Result != nil {
				r := *t.Result
				if len(r) > 500 {
					r = r[:500] + "..."
				}
				entry["result"] = r
			}
			taskResults = append(taskResults, entry)
		}
	}

	// Combine and return
	allResults := append(memResults, taskResults...)

	return h.resultJSONTracked(project, agent, "query_context", map[string]any{
		"query":   query,
		"count":   len(allResults),
		"results": allResults,
	})
}

// --- Teams + Orgs Handlers ---

func (h *Handlers) HandleCreateOrg(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	slug := req.GetString("slug", "")
	description := req.GetString("description", "")

	if name == "" || slug == "" {
		return mcp.NewToolResultError("name and slug are required"), nil
	}

	org, err := h.db.CreateOrg(name, slug, description)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create org: %v", err)), nil
	}
	return h.resultJSONTracked(resolveProject(ctx, req), "", "create_org", org)
}

func (h *Handlers) HandleListOrgs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	orgs, err := h.db.ListOrgs()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list orgs: %v", err)), nil
	}
	if orgs == nil {
		orgs = []models.Org{}
	}
	return h.resultJSONTracked(resolveProject(ctx, req), "", "list_orgs", map[string]any{"count": len(orgs), "orgs": orgs})
}

func (h *Handlers) HandleCreateTeam(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	name := req.GetString("name", "")
	slug := req.GetString("slug", "")
	description := req.GetString("description", "")
	teamType := req.GetString("type", "regular")
	orgID := optionalString(req.GetString("org_id", ""))
	parentTeamID := optionalString(req.GetString("parent_team_id", ""))

	if name == "" || slug == "" {
		return mcp.NewToolResultError("name and slug are required"), nil
	}

	// Validate type
	switch teamType {
	case "regular", "admin", "bot":
	default:
		return mcp.NewToolResultError("type must be 'regular', 'admin', or 'bot'"), nil
	}

	team, err := h.db.CreateTeam(name, slug, project, description, teamType, orgID, parentTeamID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create team: %v", err)), nil
	}
	return h.resultJSONTracked(project, "", "create_team", team)
}

func (h *Handlers) HandleListTeams(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)

	teams, err := h.db.ListTeams(project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list teams: %v", err)), nil
	}
	if teams == nil {
		teams = []models.Team{}
	}

	// Enrich with members
	result := make([]map[string]any, 0, len(teams))
	for _, t := range teams {
		members, _ := h.db.GetTeamMembers(t.ID)
		if members == nil {
			members = []models.TeamMember{}
		}
		result = append(result, map[string]any{
			"team":    t,
			"members": members,
		})
	}

	return h.resultJSONTracked(project, "", "list_teams", map[string]any{"count": len(result), "teams": result})
}

func (h *Handlers) HandleAddTeamMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	teamSlug := req.GetString("team", "")
	agentName := strings.ToLower(req.GetString("agent_name", ""))
	role := req.GetString("role", "member")

	if teamSlug == "" || agentName == "" {
		return mcp.NewToolResultError("team and agent_name are required"), nil
	}

	// Validate role
	switch role {
	case "admin", "lead", "member", "observer":
	default:
		return mcp.NewToolResultError("role must be 'admin', 'lead', 'member', or 'observer'"), nil
	}

	team, err := h.db.GetTeam(project, teamSlug)
	if err != nil || team == nil {
		return mcp.NewToolResultError(fmt.Sprintf("team '%s' not found", teamSlug)), nil
	}

	if err := h.db.AddTeamMember(team.ID, agentName, project, role); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to add member: %v", err)), nil
	}

	return h.resultJSONTracked(project, "", "add_team_member", map[string]any{
		"team":       teamSlug,
		"agent_name": agentName,
		"role":       role,
		"added":      true,
	})
}

func (h *Handlers) HandleRemoveTeamMember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	teamSlug := req.GetString("team", "")
	agentName := strings.ToLower(req.GetString("agent_name", ""))

	if teamSlug == "" || agentName == "" {
		return mcp.NewToolResultError("team and agent_name are required"), nil
	}

	team, err := h.db.GetTeam(project, teamSlug)
	if err != nil || team == nil {
		return mcp.NewToolResultError(fmt.Sprintf("team '%s' not found", teamSlug)), nil
	}

	if err := h.db.RemoveTeamMember(team.ID, agentName); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to remove member: %v", err)), nil
	}

	return h.resultJSONTracked(project, "", "remove_team_member", map[string]any{
		"team":       teamSlug,
		"agent_name": agentName,
		"removed":    true,
	})
}

func (h *Handlers) HandleGetTeamInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	teamSlug := req.GetString("team", "")
	limit := req.GetInt("limit", 50)

	if teamSlug == "" {
		return mcp.NewToolResultError("team is required"), nil
	}

	team, err := h.db.GetTeam(project, teamSlug)
	if err != nil || team == nil {
		return mcp.NewToolResultError(fmt.Sprintf("team '%s' not found", teamSlug)), nil
	}

	msgs, err := h.db.GetTeamInbox(team.ID, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get team inbox: %v", err)), nil
	}
	if msgs == nil {
		msgs = []models.Message{}
	}

	return h.resultJSONTracked(project, "", "get_team_inbox", map[string]any{
		"team":     teamSlug,
		"count":    len(msgs),
		"messages": msgs,
	})
}

func (h *Handlers) HandleAddNotifyChannel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	target := strings.ToLower(req.GetString("target", ""))

	if target == "" {
		return mcp.NewToolResultError("target is required"), nil
	}

	if err := h.db.AddNotifyChannel(agent, project, target); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to add notify channel: %v", err)), nil
	}

	return h.resultJSONTracked(project, agent, "add_notify_channel", map[string]any{
		"agent":  agent,
		"target": target,
		"added":  true,
	})
}
