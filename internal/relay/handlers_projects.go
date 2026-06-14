package relay

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

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
