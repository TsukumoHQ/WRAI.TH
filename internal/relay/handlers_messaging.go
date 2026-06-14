package relay

import (
	"agent-relay/internal/db"
	"agent-relay/internal/models"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

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

func (h *Handlers) HandleGetInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	unreadOnly := req.GetBool("unread_only", true)
	limit := clampLimit(req.GetInt("limit", 10))
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

	// Thread can hold up to 200 messages; injecting every full body unbounded
	// could dump 100k+ tokens into context. Preview-truncate (like get_inbox)
	// unless full_content=true; default to a compact markdown table.
	fullContent := req.GetBool("full_content", false)
	formatted := make([]map[string]any, len(messages))
	for i, m := range messages {
		content := m.Content
		if !fullContent && len(content) > msgContentPreview {
			content = content[:msgContentPreview] + "..."
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
		// metadata is heavy and rarely needed; only include with full_content.
		if fullContent && m.Metadata != "" && m.Metadata != "{}" {
			entry["metadata"] = m.Metadata
		}
		formatted[i] = entry
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(messages))
		for i, m := range messages {
			content, _ := formatted[i]["content"].(string)
			rows[i] = []string{m.ID, m.From, m.To, m.Type, m.Priority, m.CreatedAt, m.Subject, content}
		}
		table := renderTable([]string{"id", "from", "to", "type", "priority", "created_at", "subject", "content"}, rows)
		return h.resultTextTracked(resolveProject(ctx, req), "", "get_thread", fmt.Sprintf("%d messages in thread\n%s", len(messages), table))
	}

	return h.resultJSONTracked(resolveProject(ctx, req), "", "get_thread", map[string]any{
		"count":    len(formatted),
		"messages": formatted,
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

func (h *Handlers) HandleGetTeamInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	teamSlug := req.GetString("team", "")
	limit := clampLimit(req.GetInt("limit", 50))

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

	// Team inboxes carry verbose alert/digest bodies (~4k chars each); preview-
	// truncate unless full_content=true and default to a compact markdown table
	// so a busy team inbox can't blow up context. (Previously dumped raw
	// models.Message structs — all fields, full content.)
	fullContent := req.GetBool("full_content", false)
	formatted := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		content := m.Content
		if !fullContent && len(content) > msgContentPreview {
			content = content[:msgContentPreview] + "..."
		}
		formatted[i] = map[string]any{
			"id":         m.ID,
			"from":       m.From,
			"to":         m.To,
			"type":       m.Type,
			"subject":    m.Subject,
			"content":    content,
			"created_at": m.CreatedAt,
			"priority":   m.Priority,
		}
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(msgs))
		for i, m := range msgs {
			content, _ := formatted[i]["content"].(string)
			rows[i] = []string{m.ID, m.From, m.To, m.Type, m.Priority, m.CreatedAt, m.Subject, content}
		}
		table := renderTable([]string{"id", "from", "to", "type", "priority", "created_at", "subject", "content"}, rows)
		return h.resultTextTracked(project, "", "get_team_inbox", fmt.Sprintf("%d messages for team %s\n%s", len(msgs), teamSlug, table))
	}

	return h.resultJSONTracked(project, "", "get_team_inbox", map[string]any{
		"team":     teamSlug,
		"count":    len(msgs),
		"messages": formatted,
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
