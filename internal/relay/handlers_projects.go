package relay

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (h *Handlers) HandleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// NormalizeProject (not bare ToLower) so an underscore spelling folds to the
	// same canonical name EnsureProject stores and register_agent resolves to —
	// otherwise "synergix_prod" here would create a namespace no normalized
	// handler can ever reach, and the ListAgents cross-check below would miss the
	// agents already registered under the canonical "synergix-prod".
	name := NormalizeProject(req.GetString("name", ""))
	if name == "" {
		return toolResultError("name is required"), nil
	}
	if !validProjectName(name) {
		return toolResultError(fmt.Sprintf("invalid project name %q — use letters, digits, - or _, 1-64 chars, no leading dot/slash", name)), nil
	}
	description := req.GetString("description", "")
	cwd := req.GetString("cwd", "")

	// Create project in DB
	h.db.EnsureProject(name)

	// Check if already configured
	agents, _ := h.db.ListAgents(name)
	if len(agents) > 0 {
		return h.resultJSONTracked(h.resolveProject(ctx, req), name, "create_project", map[string]any{
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

	// Return the onboarding mega-prompt as plain text. The board/dispatch phases
	// branch on whether an external SSOT (Linear) owns the work: in mirror mode
	// the relay's native board is bypassed and tasks are authored in Linear.
	linearMode := h.getConnector().Active()
	prompt := buildOnboardingPrompt(name, description, cwd, interactive, linearMode)
	return mcp.NewToolResultText(prompt), nil
}

func (h *Handlers) HandleDeleteProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := NormalizeProject(req.GetString("project", ""))
	if project == "" {
		return toolResultError("project is required"), nil
	}

	if err := h.db.DeleteProject(project); err != nil {
		return toolResultError(fmt.Sprintf("failed to delete project: %v", err)), nil
	}

	return h.resultJSONTracked(h.resolveProject(ctx, req), "", "delete_project", map[string]any{
		"deleted": true,
		"project": project,
	})
}

func (h *Handlers) HandleArchiveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := NormalizeProject(req.GetString("project", ""))
	if project == "" {
		return toolResultError("project is required"), nil
	}

	if err := h.db.ArchiveProject(project); err != nil {
		return toolResultError(fmt.Sprintf("failed to archive project: %v", err)), nil
	}

	return h.resultJSONTracked(h.resolveProject(ctx, req), "", "archive_project", map[string]any{
		"archived": true,
		"project":  project,
	})
}

func (h *Handlers) HandleUnarchiveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := NormalizeProject(req.GetString("project", ""))
	if project == "" {
		return toolResultError("project is required"), nil
	}

	if err := h.db.UnarchiveProject(project); err != nil {
		return toolResultError(fmt.Sprintf("failed to unarchive project: %v", err)), nil
	}

	return h.resultJSONTracked(h.resolveProject(ctx, req), "", "unarchive_project", map[string]any{
		"unarchived": true,
		"project":    project,
	})
}
