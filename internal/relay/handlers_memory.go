package relay

import (
	"agent-relay/internal/db"
	"agent-relay/internal/models"
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

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

// HandleRemember records an ADR-style decision (TSU-51). Decisions are project
// memories (layer="decision"); the accepted set is surfaced at session start so
// agents stop re-litigating settled calls.
func (h *Handlers) HandleRemember(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	decision := req.GetString("decision", "")
	if decision == "" {
		return mcp.NewToolResultError("decision is required (the settled rule, one line)"), nil
	}
	rationale := req.GetString("rationale", "")
	area := req.GetString("area", "")
	tags := req.GetStringSlice("tags", nil)
	supersedes := req.GetString("supersedes", "")

	mem, err := h.db.RememberDecision(project, agent, area, decision, rationale, tags, supersedes)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to remember decision: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "memory", Action: "decision", Agent: agent, Project: project, Label: mem.Key})
	return h.resultJSONTracked(project, agent, "remember", map[string]any{"decision": mem})
}

// HandleRecallDecisions returns the project's accepted (non-superseded) decisions.
func (h *Handlers) HandleRecallDecisions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	decs, err := h.db.ListDecisions(project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to recall decisions: %v", err)), nil
	}
	return h.resultJSONTracked(project, agent, "recall_decisions", map[string]any{"decisions": decs, "count": len(decs)})
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
	limit := clampLimit(req.GetInt("limit", 20))

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
	limit := clampLimit(req.GetInt("limit", 50))

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
