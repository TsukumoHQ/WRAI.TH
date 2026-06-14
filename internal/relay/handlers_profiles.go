package relay

import (
	"agent-relay/internal/models"
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (h *Handlers) HandleRegisterProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	slug := req.GetString("slug", "")
	if slug == "" {
		return mcp.NewToolResultError("slug is required"), nil
	}
	name := req.GetString("name", "")
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	role := req.GetString("role", "")
	skills := normalizeJSONArrayParam(req, "skills")

	profile, err := h.db.RegisterProfile(project, slug, name, role, skills)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to register profile: %v", err)), nil
	}
	return h.resultJSONTracked(project, "", "register_profile", profile)
}

func (h *Handlers) HandleGetProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	slug := req.GetString("slug", "")
	if slug == "" {
		return mcp.NewToolResultError("slug is required"), nil
	}

	profile, err := h.db.GetProfile(project, slug)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get profile: %v", err)), nil
	}
	if profile == nil {
		return mcp.NewToolResultError(fmt.Sprintf("profile not found: %s", slug)), nil
	}
	return h.resultJSONTracked(project, "", "get_profile", profile)
}

func (h *Handlers) HandleListProfiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)

	profiles, err := h.db.ListProfiles(project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list profiles: %v", err)), nil
	}
	if profiles == nil {
		profiles = []models.Profile{}
	}

	return h.resultJSONTracked(project, "", "list_profiles", map[string]any{
		"count":    len(profiles),
		"profiles": profiles,
	})
}

func (h *Handlers) HandleFindProfiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := resolveProject(ctx, req)
	tag := req.GetString("skill_tag", "")
	skillName := req.GetString("skill_name", "")

	if tag == "" && skillName == "" {
		return mcp.NewToolResultError("skill_tag or skill_name is required"), nil
	}

	var profiles []models.Profile
	var err error
	searchKey := tag

	// Prefer structured skill_name (JOIN) over LIKE-based skill_tag
	if skillName != "" {
		profiles, err = h.db.FindProfilesBySkill(project, skillName)
		searchKey = skillName
	} else {
		profiles, err = h.db.FindProfilesBySkillTag(project, tag)
	}

	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to find profiles: %v", err)), nil
	}
	if profiles == nil {
		profiles = []models.Profile{}
	}

	return h.resultJSONTracked(project, "", "find_profiles", map[string]any{
		"skill":    searchKey,
		"count":    len(profiles),
		"profiles": profiles,
	})
}
