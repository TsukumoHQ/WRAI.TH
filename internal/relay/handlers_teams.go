package relay

import (
	"agent-relay/internal/models"
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

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
