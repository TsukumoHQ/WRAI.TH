package relay

import (
	"agent-relay/internal/db"
	"agent-relay/internal/ingest"
	"agent-relay/internal/models"
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// HandleWhoami finds the caller's Claude Code session by grepping transcripts for a unique salt.
func (h *Handlers) HandleWhoami(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	salt := req.GetString("salt", "")
	if salt == "" {
		return toolResultError("salt is required"), nil
	}
	if len(salt) < 5 {
		return toolResultError("salt too short — use at least 3 random words"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return toolResultError("cannot determine home dir"), nil
	}

	claudeDir := filepath.Join(home, ".claude", "projects")
	var matchFile string

	// Walk all .jsonl transcript files
	_ = filepath.Walk(claudeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if matchFile != "" {
			return filepath.SkipAll
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		// Scan from the end — salt is in recent lines. Read last 64KB.
		stat, _ := f.Stat()
		offset := stat.Size() - 65536
		if offset < 0 {
			offset = 0
		}
		_, _ = f.Seek(offset, 0)

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), salt) {
				matchFile = path
				return filepath.SkipAll
			}
		}
		return nil
	})

	if matchFile == "" {
		return toolResultError("salt not found in any transcript — make sure you wrote the salt in your conversation before calling whoami"), nil
	}

	// Extract session ID from filename (UUID.jsonl)
	base := filepath.Base(matchFile)
	sessionID := strings.TrimSuffix(base, ".jsonl")

	return resultJSON(map[string]any{
		"session_id":      sessionID,
		"transcript_path": matchFile,
	})
}

func (h *Handlers) HandleRegisterAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	name := strings.ToLower(req.GetString("name", ""))
	// Founder decision (e2f5ad14 #5): NO anonymous agents. Refuse an unnamed /
	// "anonymous" registration, and refuse the retired 'default' catch-all
	// project, with a TYPED code — BEFORE any DB write, so an anon/default
	// registration never takes the single-writer lock and can't wedge it.
	if name == "" || name == "anonymous" || project == "default" {
		return anonymousRefusedError(name, project), nil
	}
	if !validProjectName(project) {
		return toolResultError(fmt.Sprintf("invalid project name %q — use letters, digits, - or _, 1-64 chars, no leading dot/slash", project)), nil
	}
	if !h.allowRegister(project, name) {
		return toolResultError("rate limited: too many register_agent calls for this identity, slow down"), nil
	}
	role := req.GetString("role", "")
	description := req.GetString("description", "")
	reportsTo := optionalStringLower(req.GetString("reports_to", ""))
	profileSlug := optionalString(req.GetString("profile_slug", ""))
	isExecutive := req.GetBool("is_executive", false)
	sessionID := optionalString(req.GetString("session_id", ""))
	interestTags := req.GetString("interest_tags", "[]")
	maxContextBytes := req.GetInt("max_context_bytes", 16384)
	isService := req.GetBool("is_service", false)

	// Detect which identity fields were actually provided. GetString/GetBool conflate an
	// omitted param with an explicitly-empty one, so presence is read from the raw args.
	// On a respawn, omitted fields are preserved (not clobbered) by the DB layer.
	args := req.GetArguments()
	_, reportsToSet := args["reports_to"]
	_, profileSlugSet := args["profile_slug"]
	_, isExecutiveSet := args["is_executive"]
	_, sessionIDSet := args["session_id"]
	_, isServiceSet := args["is_service"]
	opts := db.RegisterOptions{
		ReportsToSet:   reportsToSet,
		ProfileSlugSet: profileSlugSet,
		IsExecutiveSet: isExecutiveSet,
		SessionIDSet:   sessionIDSet,
		IsService:      isService,
		IsServiceSet:   isServiceSet,
	}

	agent, isRespawn, err := h.db.RegisterAgent(project, name, role, description, reportsTo, profileSlug, isExecutive, sessionID, interestTags, maxContextBytes, opts)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to register agent: %v", err)), nil
	}

	// Bind the worktree cwd → agent so a SessionStart hook can re-attach a rotated
	// session_id after /clear (cwd is the stable key; session_id is not). ClaimCwd
	// records this agent's own binding WITHOUT evicting any other active agent
	// already on the same cwd — teams deliberately co-locate 2+ agents on one
	// worktree (teams.json), so cwd is not a 1-agent lock. It returns those other
	// names, if any, purely as an informational cohabitant list.
	var cwdCohabitants []string
	if cwd := strings.TrimSpace(req.GetString("cwd", "")); cwd != "" {
		var claimErr error
		cwdCohabitants, claimErr = h.db.ClaimCwd(project, name, cwd)
		if claimErr != nil {
			// Non-fatal: registration already succeeded. But a failed bind leaves
			// this agent's cwd column unset — a ghost with no cwd, no signal — so
			// this must not be silent.
			log.Printf("register_agent: claim cwd failed for %s/%s (cwd=%s): %v", project, name, cwd, claimErr)
		}
	}

	// Use the effective (post-merge) executive flag and profile slug so a respawn that
	// omits these still drives the leadership-team side effect and session_context.
	isExecutive = agent.IsExecutive
	profileSlug = agent.ProfileSlug

	// Auto-create admin team + add executive agent (fixes broadcast permission UX)
	var autoAdminTeam *string
	if isExecutive {
		adminTeam, _ := h.db.GetTeam(project, "leadership")
		if adminTeam == nil {
			adminTeam, _ = h.db.CreateTeam("Leadership", "leadership", project, "Auto-created admin team for executive agents", "admin", nil, nil)
		}
		if adminTeam != nil {
			_ = h.db.AddTeamMember(adminTeam.ID, name, project, "admin")
			autoAdminTeam = &adminTeam.Slug
		}
	}

	// Register the session for push notifications
	if sess := sessionFromContext(ctx); sess != nil {
		h.registry.Register(project, name, sess.SessionID())
	}

	// Build session_context for the response (Phase 2: boot-in-register)
	sessionCtx := h.buildSessionContext(project, name, profileSlug)
	sessionCtx["is_respawn"] = isRespawn

	action := "register"
	if isRespawn {
		action = "respawn"
	}
	h.events.Emit(MCPEvent{Type: "register", Action: action, Agent: name, Project: project, Label: role})

	resp := map[string]any{
		"agent":           agent,
		"session_context": sessionCtx,
	}
	if autoAdminTeam != nil {
		resp["auto_admin_team"] = *autoAdminTeam
		resp["hint"] = "You were auto-added to the 'leadership' admin team (broadcast enabled). Use send_message(to='*') to broadcast."
	}
	if len(cwdCohabitants) > 0 {
		// Informational only: this cwd is also held by other active agents. Normal
		// for a team sharing a worktree by design (teams.json) — nothing is cleared
		// or displaced. Only worth acting on if one of these is actually a stale
		// predecessor of this same agent, in which case deactivate_agent it.
		resp["cwd_shared_with"] = cwdCohabitants
		resp["cwd_shared_message"] = fmt.Sprintf("cwd is also held by %v — expected if you're on a team sharing this worktree. If one of those is a stale predecessor of you, deactivate_agent it.", cwdCohabitants)
		h.events.Emit(MCPEvent{Type: "register", Action: "cwd_shared", Agent: name, Project: project, Label: strings.Join(cwdCohabitants, ",")})
	}
	return h.resultJSONTracked(project, name, "register_agent", resp)
}

// HandleIdentityCheck is the read-only identity-integrity check (companion to
// is_eligible): can this name be reliably woken/addressed, or is it a ghost /
// cwd-collision that silently drops wakes and messages? Defaults to the caller.
func (h *Handlers) HandleIdentityCheck(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	caller := resolveAgent(ctx, req)
	name := strings.ToLower(strings.TrimSpace(req.GetString("agent", "")))
	if name == "" {
		name = strings.ToLower(caller)
	}
	if name == "" {
		return toolResultError("agent is required — pass agent=<name> (or as=<name> to check yourself)"), nil
	}
	v, err := h.db.IdentityCheck(project, name)
	if err != nil {
		return toolResultError(fmt.Sprintf("identity check failed: %v", err)), nil
	}
	return h.resultJSONTracked(project, caller, "identity_check", map[string]any{
		"name":               v.Name,
		"registered":         v.Registered,
		"cwd":                v.Cwd,
		"bound_uniquely":     v.BoundUniquely,
		"ghost":              v.Ghost,
		"conflicting_agents": v.ConflictingAgents,
		"reason":             v.Reason,
	})
}

// HandleIsEligible is the read-only sender-eligibility check (T2). A client
// calls it and PARKS on eligible=false instead of hot-looping a send that would
// fail with the typed SENDER_INACTIVE refusal. No write, so it is deliberately
// left off the mutating/guarded tool sets — an unregistered caller can still ask
// about itself and get {eligible:false, reason:"unregistered"}. Same verdict as
// the send path, because both call db.SenderEligibility.
func (h *Handlers) HandleIsEligible(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	name := strings.ToLower(req.GetString("agent", ""))
	if name == "" {
		name = strings.ToLower(resolveAgent(ctx, req))
	}
	if name == "" {
		return toolResultError("agent is required — pass agent=<name> (or as=<name> to check yourself)"), nil
	}
	agent, err := h.db.GetAgent(project, name)
	if err != nil {
		return toolResultError(fmt.Sprintf("eligibility check failed: %v", err)), nil
	}
	eligible, reason := db.SenderEligibility(agent)
	return resultJSON(map[string]any{
		"agent":    name,
		"project":  project,
		"eligible": eligible,
		"reason":   reason,
	})
}

func (h *Handlers) HandleListAgents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)

	agents, err := h.db.ListAgents(project)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to list agents: %v", err)), nil
	}
	if agents == nil {
		agents = []models.Agent{}
	}

	// Enrich with live activity from ingester
	var sessions []ingest.SessionState
	if h.ingester != nil {
		sessions = h.ingester.GetSessions()
	}
	sessionByID := make(map[string]ingest.SessionState)
	for _, s := range sessions {
		sessionByID[s.SessionID] = s
	}

	// Curated list view: identity + liveness only. Full records (session_id,
	// interest_tags, max_context_bytes, timestamps) stay on the REST API.
	type agentEntry struct {
		Name        string `json:"name"`
		Role        string `json:"role,omitempty"`
		Description string `json:"description,omitempty"`
		Status      string `json:"status"`
		IsExecutive bool   `json:"is_executive,omitempty"`
		ReportsTo   string `json:"reports_to,omitempty"`
		ProfileSlug string `json:"profile_slug,omitempty"`
		LastSeen    string `json:"last_seen"`
		Activity    string `json:"activity,omitempty"`
	}

	result := make([]agentEntry, 0, len(agents))
	for _, a := range agents {
		// Truncate description to keep the list payload bounded; full bio is on
		// the agent's profile / get_profile.
		if len(a.Description) > 200 {
			a.Description = a.Description[:200] + "…"
		}
		entry := agentEntry{
			Name:        a.Name,
			Role:        a.Role,
			Description: a.Description,
			Status:      a.Status,
			IsExecutive: a.IsExecutive,
			ReportsTo:   strOrDash(a.ReportsTo),
			ProfileSlug: strOrDash(a.ProfileSlug),
			LastSeen:    a.LastSeen,
		}
		if a.SessionID != nil {
			if s, ok := sessionByID[*a.SessionID]; ok {
				entry.Activity = string(s.Activity)
			}
		}
		result = append(result, entry)
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(result))
		for i, a := range result {
			exec := ""
			if a.IsExecutive {
				exec = "yes"
			}
			rows[i] = []string{a.Name, a.Role, a.Status, exec, a.LastSeen, a.Activity, a.Description}
		}
		table := renderTable([]string{"name", "role", "status", "executive", "last_seen", "activity", "description"}, rows)
		return h.resultTextTracked(project, "", "list_agents", fmt.Sprintf("%d agents\n%s", len(result), table))
	}

	return h.resultJSONTracked(project, "", "list_agents", map[string]any{
		"count":  len(result),
		"agents": result,
	})
}

func (h *Handlers) HandleDeactivateAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	name := strings.ToLower(req.GetString("name", ""))
	if name == "" {
		return toolResultError("name is required"), nil
	}

	if err := h.db.DeactivateAgent(project, name); err != nil {
		return toolResultError(fmt.Sprintf("failed to deactivate agent: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "register", Action: "deactivate", Agent: name, Project: project})
	h.cascadeDeactivatedAgent(project, name)

	return h.resultJSONTracked(project, name, "deactivate_agent", map[string]any{
		"deactivated": true,
		"agent":       name,
	})
}

func (h *Handlers) HandleDeleteAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	name := strings.ToLower(req.GetString("name", ""))
	if name == "" {
		return toolResultError("name is required"), nil
	}

	if err := h.db.DeleteAgent(project, name); err != nil {
		return toolResultError(fmt.Sprintf("failed to delete agent: %v", err)), nil
	}
	h.cascadeDeactivatedAgent(project, name)

	return h.resultJSONTracked(project, name, "delete_agent", map[string]any{
		"deleted": true,
		"agent":   name,
	})
}

// cascadeDeactivatedAgent runs the referential-integrity soft-cascade (Phase 2,
// task 536ecc40) after an agent is deactivated or deleted: it frees the leased
// tasks the dead agent still held (-> pending) and nudges the profile so a live
// agent re-claims, marks its non-leased assignments limbo, and soft-closes its
// memberships. Best-effort + non-fatal — a cascade failure never fails the
// deactivate/delete (the periodic lease + integrity sweeps are the backstop).
func (h *Handlers) cascadeDeactivatedAgent(project, name string) {
	res, err := h.db.CascadeAgentDeactivation(project, name)
	if err != nil {
		log.Printf("integrity cascade (%s/%s): %v", project, name, err)
		return
	}
	for _, s := range res.Released {
		// Mirror the expired-lease sweep: announce the recovery and nudge a live
		// agent of the profile to pick up the now-pending task.
		h.events.Emit(MCPEvent{
			Type: "task", Action: "lease_reclaimed", Agent: "relay-cascade",
			Project: s.Project, Target: s.Profile, Label: s.Title, Priority: s.Priority,
		})
		if h.registry != nil && s.Profile != "" {
			h.registry.NotifyProfile(s.Project, s.Profile, "relay-cascade",
				"Task requeued (holder "+s.From+" deactivated): "+s.Title, s.TaskID)
		}
	}
	if len(res.Released) > 0 || res.MarkedLimbo > 0 || res.LeftTeams > 0 || res.LeftConvos > 0 {
		log.Printf("integrity cascade (%s/%s): released=%d marked_limbo=%d left_teams=%d left_convos=%d",
			project, name, len(res.Released), res.MarkedLimbo, res.LeftTeams, res.LeftConvos)
	}
}

func (h *Handlers) HandleSleepAgent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)

	if err := h.db.SleepAgent(project, agent); err != nil {
		return toolResultError(fmt.Sprintf("failed to sleep agent: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "register", Action: "sleep", Agent: agent, Project: project})

	return h.resultJSONTracked(project, agent, "sleep_agent", map[string]any{
		"status": "sleeping",
		"agent":  agent,
	})
}
