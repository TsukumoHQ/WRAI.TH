package relay

import (
	"agent-relay/internal/models"
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

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
