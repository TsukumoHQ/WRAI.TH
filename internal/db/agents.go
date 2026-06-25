package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const agentColumns = "id, name, role, description, registered_at, last_seen, project, reports_to, profile_slug, status, deactivated_at, is_executive, session_id, interest_tags, max_context_bytes, avatar_url"

func scanAgent(row interface{ Scan(...any) error }) (models.Agent, error) {
	var a models.Agent
	err := row.Scan(&a.ID, &a.Name, &a.Role, &a.Description, &a.RegisteredAt, &a.LastSeen, &a.Project, &a.ReportsTo, &a.ProfileSlug, &a.Status, &a.DeactivatedAt, &a.IsExecutive, &a.SessionID, &a.InterestTags, &a.MaxContextBytes, &a.AvatarURL)
	return a, err
}

// RegisterOptions carries presence flags for identity fields whose absence must be
// distinguished from an explicit clear. At the MCP layer, an omitted optional param and
// an explicitly-empty one both arrive as a nil *string (or zero bool), so the caller sets
// the *Set flag only when the field was actually provided. On a re-register (respawn),
// fields that were NOT provided are preserved from the existing row instead of being
// clobbered to NULL/false. The flags are ignored on the initial insert.
type RegisterOptions struct {
	ReportsToSet   bool
	ProfileSlugSet bool
	IsExecutiveSet bool
	SessionIDSet   bool
}

func (d *DB) RegisterAgent(project, name, role, description string, reportsTo, profileSlug *string, isExecutive bool, sessionID *string, interestTags string, maxContextBytes int, opts RegisterOptions) (*models.Agent, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if interestTags == "" {
		interestTags = "[]"
	}
	if maxContextBytes <= 0 {
		maxContextBytes = 16384
	}

	// Ensure the project exists (auto-create with random planet on first use)
	d.EnsureProject(project)

	a, err := scanAgent(d.conn.QueryRow("SELECT "+agentColumns+" FROM agents WHERE name = ? AND project = ?", name, project))
	if err == sql.ErrNoRows {
		agent := &models.Agent{
			ID:              uuid.New().String(),
			Name:            name,
			Role:            role,
			Description:     description,
			RegisteredAt:    now,
			LastSeen:        now,
			Project:         project,
			ReportsTo:       reportsTo,
			ProfileSlug:     profileSlug,
			Status:          "active",
			IsExecutive:     isExecutive,
			SessionID:       sessionID,
			InterestTags:    interestTags,
			MaxContextBytes: maxContextBytes,
		}
		_, err := d.conn.Exec(
			"INSERT INTO agents ("+agentColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			agent.ID, agent.Name, agent.Role, agent.Description, agent.RegisteredAt, agent.LastSeen,
			agent.Project, agent.ReportsTo, agent.ProfileSlug, agent.Status, agent.DeactivatedAt, agent.IsExecutive, agent.SessionID,
			agent.InterestTags, agent.MaxContextBytes, agent.AvatarURL,
		)
		if err != nil {
			return nil, false, fmt.Errorf("insert agent: %w", err)
		}
		return agent, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("query agent: %w", err)
	}

	// Existing agent — this is a respawn. Preserve identity fields that were NOT
	// provided on this call (reports_to, profile_slug, is_executive, session_id), so a
	// bare re-register (e.g. the agent's own in-pane /relay register, which omits
	// profile_slug) does not clobber values set by the orchestrator. Fields like role,
	// description, interest_tags and max_context_bytes always update — updating them is
	// the point of a respawn. To clear an identity field, use the dedicated flows
	// (deactivate_agent / delete_agent / remove_team_member).
	if !opts.ReportsToSet {
		reportsTo = a.ReportsTo
	}
	if !opts.ProfileSlugSet {
		profileSlug = a.ProfileSlug
	}
	if !opts.IsExecutiveSet {
		isExecutive = a.IsExecutive
	}
	if !opts.SessionIDSet {
		sessionID = a.SessionID
	}

	_, err = d.conn.Exec(
		"UPDATE agents SET role = ?, description = ?, last_seen = ?, reports_to = ?, profile_slug = ?, is_executive = ?, session_id = ?, interest_tags = ?, max_context_bytes = ?, status = 'active', deactivated_at = NULL WHERE name = ? AND project = ?",
		role, description, now, reportsTo, profileSlug, isExecutive, sessionID, interestTags, maxContextBytes, name, project,
	)
	if err != nil {
		return nil, false, fmt.Errorf("update agent: %w", err)
	}
	a.Role = role
	a.Description = description
	a.LastSeen = now
	a.ReportsTo = reportsTo
	a.ProfileSlug = profileSlug
	a.IsExecutive = isExecutive
	a.SessionID = sessionID
	a.InterestTags = interestTags
	a.MaxContextBytes = maxContextBytes
	a.Status = "active"
	a.DeactivatedAt = nil
	return &a, true, nil
}

func (d *DB) TouchAgent(project, name string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.conn.Exec("UPDATE agents SET last_seen = ? WHERE name = ? AND project = ?", now, name, project)
	return err
}

func (d *DB) ListAgents(project string) ([]models.Agent, error) {
	rows, err := d.ro().Query("SELECT "+agentColumns+" FROM agents WHERE project = ? AND status IN ('active', 'sleeping', 'inactive') ORDER BY name LIMIT 500", project)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var agents []models.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// MarkStaleAgentsInactive marks agents whose last_seen is older than the given duration as inactive.
func (d *DB) MarkStaleAgentsInactive(maxAge time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.conn.Exec(
		"UPDATE agents SET status = 'inactive', deactivated_at = ? WHERE last_seen < ? AND status = 'active'",
		now, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("mark stale agents inactive: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// SleepAgent sets an agent to sleeping status (visible but not working).
func (d *DB) SleepAgent(project, name string) error {
	_, err := d.conn.Exec(
		"UPDATE agents SET status = 'sleeping' WHERE name = ? AND project = ? AND status = 'active'",
		name, project,
	)
	return err
}

// DeactivateAgent explicitly deactivates an agent.
func (d *DB) DeactivateAgent(project, name string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.conn.Exec(
		"UPDATE agents SET status = 'inactive', deactivated_at = ? WHERE name = ? AND project = ? AND status IN ('active', 'sleeping')",
		now, name, project,
	)
	return err
}

// DeleteAgent soft-deletes an agent (disappears from UI, stays in DB).
func (d *DB) DeleteAgent(project, name string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.conn.Exec(
		"UPDATE agents SET status = 'deleted', deactivated_at = ? WHERE name = ? AND project = ?",
		now, name, project,
	)
	return err
}

// GetAgentsByProfile returns active agents running a given profile slug.
func (d *DB) GetAgentsByProfile(project, profileSlug string) ([]models.Agent, error) {
	rows, err := d.ro().Query(
		"SELECT "+agentColumns+" FROM agents WHERE project = ? AND profile_slug = ? AND status = 'active'",
		project, profileSlug,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var agents []models.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// FindActiveAgentsBySkill returns active agents whose profile is linked to the given skill.
func (d *DB) FindActiveAgentsBySkill(project, skillName string) ([]models.Agent, error) {
	rows, err := d.ro().Query(
		`SELECT a.id, a.name, a.role, a.description, a.registered_at, a.last_seen, a.project,
		 a.reports_to, a.profile_slug, a.status, a.deactivated_at, a.is_executive, a.session_id,
		 a.interest_tags, a.max_context_bytes
		 FROM agents a
		 JOIN profiles p ON p.slug = a.profile_slug AND p.project = a.project
		 JOIN profile_skills ps ON ps.profile_id = p.id
		 JOIN skills s ON s.id = ps.skill_id
		 WHERE a.project = ? AND s.name = ? AND a.status = 'active'
		 ORDER BY ps.proficiency, a.name`,
		project, skillName,
	)
	if err != nil {
		return nil, fmt.Errorf("find agents by skill: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var agents []models.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			continue
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (d *DB) GetAgent(project, name string) (*models.Agent, error) {
	a, err := scanAgent(d.ro().QueryRow("SELECT "+agentColumns+" FROM agents WHERE name = ? AND project = ?", name, project))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent: %w", err)
	}
	return &a, nil
}

// GetAgentBySessionID resolves the active agent bound to a Claude Code session.
// Used to attribute hook-POSTed token usage. found=false when no agent owns it.
func (d *DB) GetAgentBySessionID(sessionID string) (project, name string, found bool, err error) {
	if sessionID == "" {
		return "", "", false, nil
	}
	row := d.ro().QueryRow(
		"SELECT project, name FROM agents WHERE session_id = ? AND status = 'active' LIMIT 1",
		sessionID,
	)
	switch e := row.Scan(&project, &name); {
	case e == sql.ErrNoRows:
		return "", "", false, nil
	case e != nil:
		return "", "", false, fmt.Errorf("get agent by session: %w", e)
	}
	return project, name, true, nil
}

// SetAgentCwd records the worktree cwd for an agent — the stable key used to
// re-bind a rotating Claude Code session_id on SessionStart. No-op if the agent
// row doesn't exist yet.
func (d *DB) SetAgentCwd(project, name, cwd string) error {
	if cwd == "" {
		return nil
	}
	_, err := d.conn.Exec(
		"UPDATE agents SET cwd = ? WHERE project = ? AND name = ?",
		cwd, project, name,
	)
	if err != nil {
		return fmt.Errorf("set agent cwd: %w", err)
	}
	return nil
}

// RebindSessionByCwd points the agent bound to cwd at a new session_id (Claude
// Code rotates session_id on /clear; cwd is stable). Returns the agent's project
// and name, and found=false when no active agent owns that cwd. cwd is globally
// unique (one agent = one worktree), so no project scoping is needed.
func (d *DB) RebindSessionByCwd(cwd, sessionID string) (project, name string, found bool, err error) {
	if cwd == "" {
		return "", "", false, nil
	}
	row := d.ro().QueryRow(
		"SELECT project, name FROM agents WHERE cwd = ? AND status = 'active' LIMIT 1",
		cwd,
	)
	switch e := row.Scan(&project, &name); {
	case e == sql.ErrNoRows:
		return "", "", false, nil
	case e != nil:
		return "", "", false, fmt.Errorf("rebind session by cwd: %w", e)
	}
	if _, e := d.conn.Exec(
		"UPDATE agents SET session_id = ? WHERE project = ? AND name = ?",
		sessionID, project, name,
	); e != nil {
		return "", "", false, fmt.Errorf("rebind session by cwd: %w", e)
	}
	return project, name, true, nil
}

// GetOrgTree returns all active agents ordered for tree display (managers first).
func (d *DB) GetOrgTree(project string) ([]models.Agent, error) {
	rows, err := d.ro().Query(
		"SELECT "+agentColumns+" FROM agents WHERE project = ? AND status = 'active' ORDER BY reports_to IS NULL DESC, reports_to, name",
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("get org tree: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var agents []models.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// GetKnownSessionIDs returns the set of session_ids from all registered agents.
func (d *DB) GetKnownSessionIDs() map[string]bool {
	rows, err := d.ro().Query("SELECT session_id FROM agents WHERE session_id IS NOT NULL AND session_id != ''")
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	ids := make(map[string]bool)
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err == nil {
			ids[sid] = true
		}
	}
	return ids
}

// SetAgentAvatar sets (or clears, with "") the agent's avatar image URL.
func (d *DB) SetAgentAvatar(project, name, url string) error {
	var v *string
	if url != "" {
		v = &url
	}
	_, err := d.conn.Exec("UPDATE agents SET avatar_url = ? WHERE project = ? AND name = ?", v, project, name)
	return err
}
