package relay

import (
	"agent-relay/internal/models"
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

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

func (h *Handlers) HandleQueryContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := clampLimit(req.GetInt("limit", 10))

	// Source 1: memories via FTS5
	memories, err := h.db.SearchMemory(project, agent, query, nil, "", limit)
	if err != nil {
		memories = []models.Memory{}
	}

	// Truncate memory values
	memResults := make([]map[string]any, len(memories))
	for i, m := range memories {
		val := m.Value
		if v, truncated := truncatePreview(val, 500); truncated {
			val = v + "..."
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
