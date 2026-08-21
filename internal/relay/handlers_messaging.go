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
	project := h.resolveProject(ctx, req)
	from := resolveAgent(ctx, req)
	to := strings.ToLower(req.GetString("to", ""))
	msgType := req.GetString("type", "notification")
	subject := req.GetString("subject", "")
	content := req.GetString("content", "")
	if content == "" {
		return toolResultError("content is required"), nil
	}

	metadata := req.GetString("metadata", "{}")
	replyTo := optionalString(req.GetString("reply_to", ""))
	conversationID := optionalString(req.GetString("conversation_id", ""))
	priority := mapPriority(req.GetString("priority", "P2"))
	ttlSeconds := req.GetInt("ttl_seconds", 14400)
	targetProject := NormalizeProject(req.GetString("target_project", ""))

	// Comms-discipline action tag (DEC-relay-comms-discipline-1). Optional: ""
	// lets the DB derive it from (type, reply_to). 'none' routes no-wake.
	actionRequired := req.GetString("action_required", "")
	if actionRequired != "" {
		switch actionRequired {
		case "ask", "do", "decide", "none":
		default:
			return validationError(CodeInvalidArgument, "action_required must be one of ask|do|decide|none"), nil
		}
	}

	// A message to a project nobody ever created is a black hole: the row
	// is written (messages have no project FK), no inbox ever reads it, and
	// the sender walks away believing it was delivered. Fail loud with the
	// nearest real name instead. "default" stays grandfathered — it is the
	// CLI's compiled fallback, not a caller mistake.
	if project != "default" {
		if known, err := h.db.GetProject(project); err == nil && known == nil {
			if sugg := h.suggestProject(project); sugg != "" && sugg != project {
				return toolResultError(fmt.Sprintf("unknown project %q — did you mean %q? (projects are created by register_agent or create_project)", project, sugg)), nil
			}
			return toolResultError(fmt.Sprintf("unknown project %q — register_agent or create_project first", project)), nil
		}
	}

	// Quota check: messages
	if qErr := h.db.CheckQuotaError(project, from, "messages"); qErr != "" {
		return toolResultError(qErr), nil
	}

	// Federated DM: recipient addressed as "name@peerlabel" routes to a peer
	// relay. Checked before local resolution so a peer address is never mistaken
	// for a local agent. Direct DMs only (splitPeerAddr already excludes team:/
	// conversation:/broadcast). No-op when federation is disabled or "to" has no
	// "@peer" suffix.
	if h.federation.Enabled() {
		if name, peerLabel, ok := splitPeerAddr(to); ok {
			return h.sendFederated(ctx, project, from, peerLabel, name, msgType, subject, content, priority, ttlSeconds, replyTo)
		}
	}

	// Cross-project DM: delivered to a peer executive in a different project.
	// MVP scope: direct messages only (no broadcast, no team, no conversation).
	// Both sender and recipient must be registered with is_executive=true.
	if targetProject != "" && targetProject != project {
		if to == "" {
			return toolResultError("target_project requires a 'to' agent name"), nil
		}
		if to == "*" || strings.HasPrefix(to, "team:") || conversationID != nil {
			return toolResultError("cross-project messaging is limited to direct DMs (no broadcast, no team:, no conversation_id)"), nil
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
			return toolResultError(fmt.Sprintf("failed to check membership: %v", err)), nil
		}
		if !isMember {
			return toolResultError("you are not a member of this conversation"), nil
		}
		to = "" // no single recipient for conversation messages
	} else if to == "" {
		return toolResultError("to is required (or provide conversation_id)"), nil
	}

	// Permission check: only enforce when teams are configured (bypass for "user" — always reachable)
	if conversationID == nil && to != "*" && to != "user" && !strings.HasPrefix(to, "team:") {
		hasTeams, _ := h.db.HasTeams(project)
		if hasTeams {
			allowed, err := h.db.CanMessage(project, from, to)
			if err != nil {
				return toolResultError(fmt.Sprintf("permission check failed: %v", err)), nil
			}
			if !allowed {
				// A name nobody has ever registered AND nobody ever dispatched a
				// task to is a genuine unknown — refuse. But a name that was
				// dispatched a task (profile_slug = to) and simply hasn't called
				// register_agent yet is a boot-race, not a bad address: queue the
				// send instead of hard-refusing. Deliveries key on to_agent by
				// name (no agent-row FK), so it surfaces the moment the recipient
				// registers and calls get_inbox — no separate flush required.
				expected := false
				if existing, _ := h.db.GetAgent(project, to); existing == nil {
					expected = h.db.RecipientIsFleetExpected(project, to)
				}
				if !expected {
					return toolResultError(fmt.Sprintf("not authorized to message '%s' — no shared team, reports_to chain, notify channel, or reply-path (they haven't messaged you). Ask an admin/executive to relay, or have '%s' message you first (that grants a scoped reply-path).", to, to)), nil
				}
			}
		}
	}

	// Team addressing: to="team:slug" → fan out to team members + team_inbox
	if strings.HasPrefix(to, "team:") {
		teamSlug := strings.TrimPrefix(to, "team:")
		team, err := h.db.ResolveTeamSlug(project, teamSlug)
		if err != nil || team == nil {
			return toolResultError(fmt.Sprintf("team '%s' not found", teamSlug)), nil
		}

		// Resolve team recipients first, then insert message + deliveries atomically.
		members, _ := h.db.GetTeamMemberNames(team.ID)

		// An empty (or all-inactive) team is a silent black hole: the send would
		// look successful yet reach nobody. Fail loudly so the condition is
		// observable to the sender rather than swallowed (issue #150). A team
		// whose only member is the sender is NOT this case — the message still
		// lands in the team inbox as a record.
		if len(members) == 0 {
			return toolResultError(fmt.Sprintf(
				"team '%s' has no members — message not sent (it would reach nobody). Add members with add_team_member, or delete_team to retire the channel.", teamSlug)), nil
		}

		var recipients []string
		for _, member := range members {
			if member != from {
				recipients = append(recipients, member)
			}
		}

		msg, err := h.db.InsertMessageWithDeliveries(project, from, to, msgType, subject, content, metadata, priority, ttlSeconds, replyTo, conversationID, recipients, actionRequired)
		if err != nil {
			return toolResultError(fmt.Sprintf("failed to send message: %v", err)), nil
		}

		// Best-effort bookkeeping + notifications after the durable write.
		_ = h.db.AddToTeamInbox(team.ID, msg.ID)
		for _, member := range recipients {
			h.registry.Notify(project, member, from, subject, msg.ID)
		}

		return h.resultJSONTracked(project, from, "send_message", msg)
	}

	// Broadcast permission: when teams exist, only admin team members can broadcast
	if to == "*" {
		hasTeams, _ := h.db.HasTeams(project)
		if hasTeams {
			allowed, _ := h.db.CanMessage(project, from, "*")
			if !allowed {
				return toolResultError("broadcast requires membership in an 'admin' type team. Fix: register with is_executive=true (auto-creates admin team), or manually: create_team(type='admin') then add_team_member()"), nil
			}
		}
	}

	// Resolve fan-out recipients first, then insert message + deliveries atomically
	// so a message can never be persisted without its deliveries (silent non-delivery).
	recipients, _ := h.db.ResolveRecipients(project, to, from, conversationID)
	msg, err := h.db.InsertMessageWithDeliveries(project, from, to, msgType, subject, content, metadata, priority, ttlSeconds, replyTo, conversationID, recipients, actionRequired)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to send message: %v", err)), nil
	}

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
	h.events.Emit(MCPEvent{Type: "message", Action: action, Agent: from, Project: project, Target: to, Label: subject, Priority: priority, MsgType: msgType})

	return h.resultJSONTracked(project, from, "send_message", msg)
}

// HandleSendStatus posts a typed-status report (DEC-relay-comms-discipline-1
// mechanism B): {done[],doing[],blockers[]} + one capped free-text note, slotted
// into the message metadata. It is ACCEPT-AND-SLOT — over-long slots/note are
// capped (see buildStatusPayload), never rejected — and always no-wake: the
// message is type 'status' with action_required forced 'none', so a listed
// blocker is SURFACED in the inbox, not a wake (Ruling-3: passively listing a
// blocker is not escalating). A P0 status still wakes via the guard-first
// predicate's P0 clause — the deliberate escape hatch. To escalate, send a
// question; this path never gates one.
func (h *Handlers) HandleSendStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	from := resolveAgent(ctx, req)
	to := strings.ToLower(req.GetString("to", ""))
	conversationID := optionalString(req.GetString("conversation_id", ""))
	if conversationID == nil && strings.HasPrefix(to, "conversation:") {
		cid := strings.TrimPrefix(to, "conversation:")
		conversationID = &cid
	}
	if conversationID == nil && to == "" {
		return toolResultError("to is required (agent name, '*', or a conversation_id)"), nil
	}
	if strings.HasPrefix(to, "team:") {
		return toolResultError("send_status targets a direct agent, '*', or a conversation — not team:<slug> (send a status to your lead, or '*' to the fleet)"), nil
	}

	priority := mapPriority(req.GetString("priority", "P2"))
	ttlSeconds := req.GetInt("ttl_seconds", 14400)

	// Accept-and-slot: a malformed done/doing/blockers arg (prose instead of an
	// array, a stray non-string item) is never silently dropped — it's folded
	// into the note valve instead, so nothing the caller sent is lost.
	doneItems, doneProse := statusSlotArg(req, "done")
	doingItems, doingProse := statusSlotArg(req, "doing")
	blockersItems, blockersProse := statusSlotArg(req, "blockers")
	note := req.GetString("note", "")
	note = foldMalformedIntoNote(note, "done", doneProse)
	note = foldMalformedIntoNote(note, "doing", doingProse)
	note = foldMalformedIntoNote(note, "blockers", blockersProse)

	content, metadata, _ := buildStatusPayload(doneItems, doingItems, blockersItems, note)

	// Unknown-project guard (mirrors send_message): a status to a project nobody
	// created is a black hole. "default" stays grandfathered.
	if project != "default" {
		if known, err := h.db.GetProject(project); err == nil && known == nil {
			return toolResultError(fmt.Sprintf("unknown project %q — register_agent or create_project first", project)), nil
		}
	}
	if qErr := h.db.CheckQuotaError(project, from, "messages"); qErr != "" {
		return toolResultError(qErr), nil
	}
	_ = h.db.TouchAgent(project, from)

	// Same permission model as send_message for the paths supported here.
	if conversationID != nil {
		isMember, err := h.db.IsConversationMember(*conversationID, from)
		if err != nil {
			return toolResultError(fmt.Sprintf("failed to check membership: %v", err)), nil
		}
		if !isMember {
			return toolResultError("you are not a member of this conversation"), nil
		}
		to = ""
	} else if to == "*" {
		if hasTeams, _ := h.db.HasTeams(project); hasTeams {
			if allowed, _ := h.db.CanMessage(project, from, "*"); !allowed {
				return toolResultError("broadcast requires membership in an 'admin' type team (register is_executive=true)"), nil
			}
		}
	} else if to != "user" {
		if hasTeams, _ := h.db.HasTeams(project); hasTeams {
			allowed, err := h.db.CanMessage(project, from, to)
			if err != nil {
				return toolResultError(fmt.Sprintf("permission check failed: %v", err)), nil
			}
			if !allowed {
				return toolResultError(fmt.Sprintf("not authorized to message '%s' — no shared team, reports_to chain, notify channel, or reply-path.", to)), nil
			}
		}
	}

	// action_required forced 'none' — a status never wakes (P0 still wakes via the
	// guard-first predicate). Recipients resolved first so the message is never
	// persisted without its deliveries.
	recipients, _ := h.db.ResolveRecipients(project, to, from, conversationID)
	msg, err := h.db.InsertMessageWithDeliveries(project, from, to, "status", "status", content, metadata, priority, ttlSeconds, nil, conversationID, recipients, "none")
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to send status: %v", err)), nil
	}

	if conversationID != nil {
		h.notifyConversation(project, *conversationID, from, "status", msg.ID)
	} else if to == "*" {
		h.registry.NotifyBroadcast(project, from, "status", msg.ID)
	} else {
		h.registry.Notify(project, to, from, "status", msg.ID)
	}

	action := "send"
	if to == "*" {
		action = "broadcast"
	} else if conversationID != nil {
		action = "conversation"
	}
	h.events.Emit(MCPEvent{Type: "message", Action: action, Agent: from, Project: project, Target: to, Label: "status", Priority: priority, MsgType: "status"})

	return h.resultJSONTracked(project, from, "send_status", msg)
}

func (h *Handlers) HandleGetInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	unreadOnly := req.GetBool("unread_only", true)
	limit := clampLimit(req.GetInt("limit", 10))
	fullContent := req.GetBool("full_content", false)
	budgetMode := req.GetBool("apply_budget", false)

	_ = h.db.TouchAgent(project, agent)

	// Expiry is flagged by the background cleanup ticker (StartCleanup); the inbox
	// queries also filter TTL-elapsed messages by timestamp, so we don't take a
	// write-lock on this hot poll path just to expire.

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
		return toolResultError(fmt.Sprintf("failed to get inbox: %v", err)), nil
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
		// Truncation safety net (TSU-73): a P0 is never truncated, and a message
		// is delivered in full on its FIRST surfacing (delivery still 'queued', or
		// legacy unread). Subsequent peeks of a longer message preview-truncate to
		// save context — the full text is one full_content=true re-fetch away,
		// because the inbox is now a non-destructive peek (the message stays unread
		// until an explicit mark_read / ack_delivery).
		firstRead := (m.DeliveryState != nil && *m.DeliveryState == "queued") ||
			(m.DeliveryState == nil && m.ReadAt == nil)
		isP0 := m.Priority == "P0"
		if !fullContent && !isP0 && !firstRead && len(content) > 300 {
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
			project := h.resolveProject(ctx, req)
			if err := h.db.AcknowledgeDeliveryByMessage(msgID, agent, project); err != nil {
				return toolResultError(fmt.Sprintf("failed to acknowledge delivery by message: %v", err)), nil
			}
			return h.resultJSONTracked(project, agent, "ack_delivery", map[string]any{"acknowledged_message_id": msgID})
		}
		return toolResultError("delivery_id is required (get it from get_inbox response — each message has a delivery_id field). If you only have the message_id, pass message_id + as + project instead."), nil
	}
	if err := h.db.AcknowledgeDelivery(deliveryID); err != nil {
		return toolResultError(fmt.Sprintf("failed to acknowledge delivery: %v", err)), nil
	}
	return h.resultJSONTracked(h.resolveProject(ctx, req), "", "ack_delivery", map[string]any{"acknowledged": deliveryID})
}

// HandleDeliveryStatus is the read path for the deliveries state machine (T4):
// list deliveries by message_id OR recipient agent, so "what was surfaced/acked"
// is auditable — ack_delivery is otherwise effectively write-only. Read-only.
func (h *Handlers) HandleDeliveryStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	messageID := req.GetString("message_id", "")
	target := req.GetString("agent", "")
	if messageID == "" && target == "" {
		return toolResultError("delivery_status requires message_id or agent"), nil
	}
	rows, err := h.db.DeliveryStatus(project, messageID, target)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to get delivery status: %v", err)), nil
	}
	if rows == nil {
		rows = []models.DeliveryStatusRow{}
	}
	return h.resultJSONTracked(project, agent, "delivery_status", map[string]any{
		"count":      len(rows),
		"deliveries": rows,
	})
}

// HandleDeadletter lists expired-unread messages (T6) — the durable record that
// a TTL-expired P0/P1 left behind. Read-only. Defaults to the caller.
func (h *Handlers) HandleDeadletter(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	target := req.GetString("agent", "")
	if target == "" {
		target = agent
	}
	limit := clampLimit(req.GetInt("limit", 50))
	rows, err := h.db.Deadletter(project, target, limit)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to list deadletter: %v", err)), nil
	}
	if rows == nil {
		rows = []models.DeadletterRow{}
	}
	return h.resultJSONTracked(project, agent, "deadletter", map[string]any{
		"agent":      target,
		"count":      len(rows),
		"deadletter": rows,
	})
}

func (h *Handlers) HandleGetThread(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	messageID := req.GetString("message_id", "")
	if messageID == "" {
		return toolResultError("message_id is required"), nil
	}

	messages, err := h.db.GetThread(messageID)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to get thread: %v", err)), nil
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

	// Attach the caller's own delivery_id per message (T4) so a viewer can ack
	// straight from a thread read, not only from get_inbox.
	project := h.resolveProject(ctx, req)
	if agent := resolveAgent(ctx, req); agent != "" && len(messages) > 0 {
		ids := make([]string, len(messages))
		for i, m := range messages {
			ids[i] = m.ID
		}
		if dmap, derr := h.db.DeliveryIDsForAgent(project, agent, ids); derr == nil {
			for i, m := range messages {
				if did, ok := dmap[m.ID]; ok {
					formatted[i]["delivery_id"] = did
				}
			}
		}
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(messages))
		for i, m := range messages {
			content, _ := formatted[i]["content"].(string)
			rows[i] = []string{m.ID, m.From, m.To, m.Type, m.Priority, m.CreatedAt, m.Subject, content}
		}
		table := renderTable([]string{"id", "from", "to", "type", "priority", "created_at", "subject", "content"}, rows)
		return h.resultTextTracked(project, "", "get_thread", fmt.Sprintf("%d messages in thread\n%s", len(messages), table))
	}

	return h.resultJSONTracked(project, "", "get_thread", map[string]any{
		"count":    len(formatted),
		"messages": formatted,
	})
}

// HandleGetMessage returns one message by ID with its full, untruncated content
// — the escape hatch for the ~300-char previews in get_inbox / get_thread /
// get_session_context (WRAITH-3). Read-only, non-mutating: it never surfaces or
// acks a delivery, so fetching a full body cannot alter the unread view.
func (h *Handlers) HandleGetMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	id := req.GetString("id", "")
	if id == "" {
		return toolResultError("id is required"), nil
	}

	msg, err := h.db.GetMessage(id)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to get message: %v", err)), nil
	}
	if msg == nil {
		// Not a full ID — try to resolve an unambiguous prefix.
		full, ferr := h.db.FindMessageByPrefix(id)
		if ferr != nil {
			return toolResultError(fmt.Sprintf("no message with id %q: %v", id, ferr)), nil
		}
		if msg, err = h.db.GetMessage(full); err != nil || msg == nil {
			return toolResultError(fmt.Sprintf("no message with id %q", id)), nil
		}
	}

	entry := map[string]any{
		"id":         msg.ID,
		"from":       msg.From,
		"to":         msg.To,
		"type":       msg.Type,
		"subject":    msg.Subject,
		"content":    msg.Content, // full, untruncated — the whole point of this tool
		"created_at": msg.CreatedAt,
		"priority":   msg.Priority,
	}
	if msg.ReplyTo != nil {
		entry["reply_to"] = *msg.ReplyTo
	}
	if msg.ConversationID != nil {
		entry["conversation_id"] = *msg.ConversationID
	}
	if msg.Metadata != "" && msg.Metadata != "{}" {
		entry["metadata"] = msg.Metadata
	}
	// Attach the caller's own delivery_id (T4) so the full-body read is ack-able.
	if agent := resolveAgent(ctx, req); agent != "" {
		if dmap, derr := h.db.DeliveryIDsForAgent(project, agent, []string{msg.ID}); derr == nil {
			if did, ok := dmap[msg.ID]; ok {
				entry["delivery_id"] = did
			}
		}
	}
	return h.resultJSONTracked(project, "", "get_message", entry)
}

func (h *Handlers) HandleMarkRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)

	// Support marking a whole conversation as read
	convID := req.GetString("conversation_id", "")
	if convID != "" {
		if err := h.db.MarkConversationRead(convID, agent); err != nil {
			return toolResultError(fmt.Sprintf("failed to mark conversation read: %v", err)), nil
		}
		// Also acknowledge the deliveries for this conversation's messages —
		// otherwise they stay queued/surfaced and keep re-surfacing (and, before
		// WRAITH-2, re-waking) until an explicit ack_delivery. Best-effort.
		_ = h.db.AcknowledgeConversationDeliveries(convID, agent, project)
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
		return toolResultError("message_ids (array) or conversation_id is required. Note: the field is plural — pass message_ids:['id1','id2'] not message_id."), nil
	}

	count, err := h.db.MarkRead(ids, agent, project)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to mark read: %v", err)), nil
	}

	// mark_read is the explicit clear for the deliveries inbox: acknowledge each
	// delivery so the message leaves the (non-destructive) unread view. Without
	// this, mark_read only wrote message_reads and the deliveries-path inbox kept
	// showing the message as unread forever (TSU-73). Best-effort per id.
	for _, id := range ids {
		_ = h.db.AcknowledgeDeliveryByMessage(id, agent, project)
	}

	return h.resultJSONTracked(project, agent, "mark_read", map[string]any{
		"marked_read": count,
	})
}

func (h *Handlers) HandleGetTeamInbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	teamSlug := req.GetString("team", "")
	limit := clampLimit(req.GetInt("limit", 50))

	if teamSlug == "" {
		return toolResultError("team is required"), nil
	}

	team, err := h.db.GetTeam(project, teamSlug)
	if err != nil || team == nil {
		return toolResultError(fmt.Sprintf("team '%s' not found", teamSlug)), nil
	}

	msgs, err := h.db.GetTeamInbox(team.ID, limit)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to get team inbox: %v", err)), nil
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

	// Attach the caller's own delivery_id per message (T4) so a team member can
	// ack a team message straight from this read.
	if agent := resolveAgent(ctx, req); agent != "" && len(msgs) > 0 {
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		if dmap, derr := h.db.DeliveryIDsForAgent(project, agent, ids); derr == nil {
			for i, m := range msgs {
				if did, ok := dmap[m.ID]; ok {
					formatted[i]["delivery_id"] = did
				}
			}
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
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	target := strings.ToLower(req.GetString("target", ""))

	if target == "" {
		return toolResultError("target is required"), nil
	}

	if err := h.db.AddNotifyChannel(agent, project, target); err != nil {
		return toolResultError(fmt.Sprintf("failed to add notify channel: %v", err)), nil
	}

	return h.resultJSONTracked(project, agent, "add_notify_channel", map[string]any{
		"agent":  agent,
		"target": target,
		"added":  true,
	})
}
