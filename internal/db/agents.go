package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const agentColumns = "id, name, role, description, registered_at, last_seen, project, reports_to, profile_slug, status, deactivated_at, is_executive, session_id, interest_tags, max_context_bytes, avatar_url, is_service"

func scanAgent(row interface{ Scan(...any) error }) (models.Agent, error) {
	var a models.Agent
	err := row.Scan(&a.ID, &a.Name, &a.Role, &a.Description, &a.RegisteredAt, &a.LastSeen, &a.Project, &a.ReportsTo, &a.ProfileSlug, &a.Status, &a.DeactivatedAt, &a.IsExecutive, &a.SessionID, &a.InterestTags, &a.MaxContextBytes, &a.AvatarURL, &a.IsService)
	return a, err
}

// SenderEligibility decides whether an agent may send (or ack). It is the single
// source of truth for the T2 sender-liveness gate, shared by the send path, the
// read-only is_eligible check, and (T4) ack_delivery — so all three agree.
//
//   - nil agent (unregistered)        → ineligible, reason "unregistered"
//   - is_service=true                 → ALWAYS eligible ("service"): a monitoring
//     or QA daemon must post feedback even when every worker is dead.
//   - status active | sleeping        → eligible (a live, non-dead participant)
//   - status inactive | deleted | …   → ineligible (the reason is the status)
//
// An ineligible verdict is surfaced to clients as the typed SENDER_INACTIVE
// refusal so they PARK instead of hot-looping a doomed retry (the fiduciaire
// dead-fleet incident).
func SenderEligibility(a *models.Agent) (eligible bool, reason string) {
	if a == nil {
		return false, "unregistered"
	}
	if a.IsService {
		return true, "service"
	}
	switch a.Status {
	case "active", "sleeping":
		return true, a.Status
	default:
		return false, a.Status
	}
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
	// IsService is the value for the is_service flag; IsServiceSet says it was
	// actually provided on this call. Carried in opts (not a positional param)
	// so the many existing RegisterAgent callers stay source-compatible — the
	// same value+presence pattern the *Set flags already use for optionals.
	IsService    bool
	IsServiceSet bool
}

func (d *DB) RegisterAgent(project, name, role, description string, reportsTo, profileSlug *string, isExecutive bool, sessionID *string, interestTags string, maxContextBytes int, opts RegisterOptions) (*models.Agent, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	isService := opts.IsService
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
			IsService:       isService,
		}
		_, err := d.conn.Exec(
			"INSERT INTO agents ("+agentColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			agent.ID, agent.Name, agent.Role, agent.Description, agent.RegisteredAt, agent.LastSeen,
			agent.Project, agent.ReportsTo, agent.ProfileSlug, agent.Status, agent.DeactivatedAt, agent.IsExecutive, agent.SessionID,
			agent.InterestTags, agent.MaxContextBytes, agent.AvatarURL, agent.IsService,
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
	if !opts.IsServiceSet {
		isService = a.IsService
	}

	_, err = d.conn.Exec(
		"UPDATE agents SET role = ?, description = ?, last_seen = ?, reports_to = ?, profile_slug = ?, is_executive = ?, session_id = ?, interest_tags = ?, max_context_bytes = ?, is_service = ?, status = 'active', deactivated_at = NULL WHERE name = ? AND project = ?",
		role, description, now, reportsTo, profileSlug, isExecutive, sessionID, interestTags, maxContextBytes, isService, name, project,
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
	a.IsService = isService
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
		// Column list MUST stay in lockstep with scanAgent (17 columns): a SELECT
		// that scans fewer than scanAgent expects errors on every row and the
		// `continue` below silently drops the whole result set. avatar_url +
		// is_service were the drift caught fixing T2.
		`SELECT a.id, a.name, a.role, a.description, a.registered_at, a.last_seen, a.project,
		 a.reports_to, a.profile_slug, a.status, a.deactivated_at, a.is_executive, a.session_id,
		 a.interest_tags, a.max_context_bytes, a.avatar_url, a.is_service
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
	// status != 'deleted' (not '= active'): an agent working hard locally only
	// bumps last_seen on relay tool calls, so the 30-min sweep can mark it
	// 'inactive' mid-work. Resolving only 'active' rows then dropped its live
	// activity → the board showed it offline while it was clearly busy. Resolve
	// any non-deleted agent; liveness is decided by the live session, not status.
	row := d.ro().QueryRow(
		"SELECT project, name FROM agents WHERE session_id = ? AND status != 'deleted' LIMIT 1",
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

// RebindSession points an agent at a new session_id (Claude Code rotates
// session_id on /clear; cwd is stable). Identity is resolved in this order:
//
//  1. agentName, when non-empty — the launcher forwarded RELAY_AGENT and knows
//     exactly which agent it is starting. cwd, when present, scopes the project
//     so a name reused across projects stays distinct. This is the correct path
//     for fleets that deliberately share one worktree.
//  2. cwd alone — only safe when cwd uniquely identifies an agent. If more than
//     one non-deleted agent shares cwd the key is AMBIGUOUS: the function refuses
//     to guess (found=false, ambiguous=true) rather than bind a wrong identity
//     and detach a correctly-bound session. A silent miss is recoverable; a
//     confident wrong identity is not.
//
// The session_id UPDATE only fires on a unique match, so an ambiguous cwd can
// never overwrite (and thus mis-attribute) another agent's live binding.
func (d *DB) RebindSession(cwd, agentName, sessionID string) (project, name string, found, ambiguous bool, err error) {
	if cwd == "" && agentName == "" {
		return "", "", false, false, nil
	}

	var row *sql.Row
	switch {
	case agentName != "" && cwd != "":
		row = d.ro().QueryRow(
			"SELECT project, name FROM agents WHERE name = ? AND cwd = ? AND status != 'deleted' LIMIT 1",
			agentName, cwd)
	case agentName != "":
		row = d.ro().QueryRow(
			"SELECT project, name FROM agents WHERE name = ? AND status != 'deleted' LIMIT 1",
			agentName)
	default:
		// cwd-only fallback: refuse ambiguous keys.
		var n int
		if e := d.ro().QueryRow(
			"SELECT COUNT(*) FROM agents WHERE cwd = ? AND status != 'deleted'", cwd,
		).Scan(&n); e != nil {
			return "", "", false, false, fmt.Errorf("rebind session: count by cwd: %w", e)
		}
		if n > 1 {
			return "", "", false, true, nil // ambiguous — caller logs, binds nothing
		}
		row = d.ro().QueryRow(
			"SELECT project, name FROM agents WHERE cwd = ? AND status != 'deleted' LIMIT 1", cwd)
	}

	switch e := row.Scan(&project, &name); {
	case e == sql.ErrNoRows:
		return "", "", false, false, nil
	case e != nil:
		return "", "", false, false, fmt.Errorf("rebind session: %w", e)
	}
	if _, e := d.conn.Exec(
		"UPDATE agents SET session_id = ? WHERE project = ? AND name = ?",
		sessionID, project, name,
	); e != nil {
		return "", "", false, false, fmt.Errorf("rebind session: update: %w", e)
	}
	return project, name, true, false, nil
}

// AgentNamesByCwd lists the non-deleted agents sharing a cwd — used to name the
// colliding agents in the warning logged when a cwd rebind key is ambiguous.
func (d *DB) AgentNamesByCwd(cwd string) ([]string, error) {
	rows, err := d.ro().Query(
		"SELECT name FROM agents WHERE cwd = ? AND status != 'deleted' ORDER BY name", cwd)
	if err != nil {
		return nil, fmt.Errorf("agent names by cwd: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("agent names by cwd: scan: %w", err)
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// ClaimCwd binds cwd to this agent as its SOLE holder in the project: it clears
// the cwd from any OTHER non-deleted same-project agent that currently holds it
// (last live registrant wins), then sets it on the claimer — all in one writer
// tx. This keeps the cwd UNAMBIGUOUS for RebindSession; an ambiguous cwd (two
// same-project names on one worktree, e.g. a 'foo' zombie + its 'foo-2' respawn)
// is exactly what makes a daemon wake hit "no local agent" and lose the message.
// Returns the displaced agent names so the caller fails-closed by FLAGGING the
// collision instead of silently accepting two live bindings on one cwd. Scoped
// to the project: a cwd deliberately shared across projects (name-based rebind)
// is left alone.
func (d *DB) ClaimCwd(project, name, cwd string) ([]string, error) {
	if cwd == "" {
		return nil, nil
	}
	tx, err := d.conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("claim cwd begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(
		"SELECT name FROM agents WHERE cwd = ? AND project = ? AND name <> ? AND status != 'deleted'",
		cwd, project, name,
	)
	if err != nil {
		return nil, fmt.Errorf("claim cwd: find holders: %w", err)
	}
	var displaced []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("claim cwd: scan: %w", err)
		}
		displaced = append(displaced, n)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim cwd: rows: %w", err)
	}

	if len(displaced) > 0 {
		// cwd is NOT NULL — clear to empty string (unbound), not NULL. An empty
		// cwd matches no real worktree, so RebindSession/IdentityCheck treat the
		// displaced agent as unbound (a ghost) rather than a live cwd holder.
		if _, err := tx.Exec(
			"UPDATE agents SET cwd = '' WHERE cwd = ? AND project = ? AND name <> ? AND status != 'deleted'",
			cwd, project, name,
		); err != nil {
			return nil, fmt.Errorf("claim cwd: displace: %w", err)
		}
	}
	if _, err := tx.Exec(
		"UPDATE agents SET cwd = ? WHERE project = ? AND name = ?", cwd, project, name,
	); err != nil {
		return nil, fmt.Errorf("claim cwd: bind: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim cwd commit: %w", err)
	}
	return displaced, nil
}

// IdentityVerdict is the identity-integrity check result (companion to the T2
// sender-eligibility verdict): can this name be reliably woken/addressed, or is
// it a ghost / cwd-collision that will silently drop wakes and messages?
type IdentityVerdict struct {
	Name              string   `json:"name"`
	Registered        bool     `json:"registered"`
	Cwd               string   `json:"cwd,omitempty"`
	BoundUniquely     bool     `json:"bound_uniquely"`
	Ghost             bool     `json:"ghost"`
	ConflictingAgents []string `json:"conflicting_agents,omitempty"`
	Reason            string   `json:"reason"`
}

// IdentityCheck reports whether an agent name is safely resolvable for wakes and
// delivery (T6 identity fail-closed). Ghost = registered but no cwd bound (or not
// registered at all) → a daemon wake by pane-name finds "no local agent" and the
// message is lost. Conflict = more than one active same-project agent shares the
// cwd → RebindSession refuses the ambiguous key. Read-only.
func (d *DB) IdentityCheck(project, name string) (IdentityVerdict, error) {
	v := IdentityVerdict{Name: name}
	if name == "" {
		v.Reason = "no name given"
		return v, nil
	}
	var cwd sql.NullString
	err := d.ro().QueryRow(
		"SELECT cwd FROM agents WHERE project = ? AND name = ? AND status != 'deleted' LIMIT 1",
		project, name,
	).Scan(&cwd)
	if err == sql.ErrNoRows {
		v.Ghost = true
		v.Reason = "unregistered — a wake for this name finds no local agent"
		return v, nil
	}
	if err != nil {
		return v, fmt.Errorf("identity check: %w", err)
	}
	v.Registered = true
	if !cwd.Valid || cwd.String == "" {
		v.Ghost = true
		v.Reason = "registered but no cwd bound — not locally wake-resolvable"
		return v, nil
	}
	v.Cwd = cwd.String

	rows, err := d.ro().Query(
		"SELECT name FROM agents WHERE cwd = ? AND project = ? AND status != 'deleted' ORDER BY name",
		cwd.String, project,
	)
	if err != nil {
		return v, fmt.Errorf("identity check: holders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return v, fmt.Errorf("identity check: scan: %w", err)
		}
		if n != name {
			v.ConflictingAgents = append(v.ConflictingAgents, n)
		}
	}
	if err := rows.Err(); err != nil {
		return v, fmt.Errorf("identity check: rows: %w", err)
	}
	if len(v.ConflictingAgents) > 0 {
		v.Reason = "cwd shared by another active agent — ambiguous binding, wakes may resolve wrong or drop"
		return v, nil
	}
	v.BoundUniquely = true
	v.Reason = "ok — uniquely bound, wake-resolvable"
	return v, nil
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

// ProjectsOfAgent returns the distinct projects holding a non-deactivated
// registration of `name`. Used to bind project-less tool calls to the
// caller's own namespace instead of the "default" catch-all.
func (d *DB) ProjectsOfAgent(name string) ([]string, error) {
	rows, err := d.ro().Query(
		"SELECT DISTINCT project FROM agents WHERE name = ? AND status IN ('active', 'sleeping', 'inactive')", name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectNames returns every project name, for near-miss suggestions.
func (d *DB) ProjectNames() ([]string, error) {
	rows, err := d.ro().Query("SELECT name FROM projects ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
