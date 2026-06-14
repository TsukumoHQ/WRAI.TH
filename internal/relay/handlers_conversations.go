package relay

import (
	"agent-relay/internal/models"
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

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
	limit := clampLimit(req.GetInt("limit", 50))

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
