package relay

import (
	"agent-relay/internal/models"
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

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
