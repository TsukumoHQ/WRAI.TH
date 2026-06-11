package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// --- Orgs ---

func (d *DB) CreateOrg(name, slug, description string) (*models.Org, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	org := &models.Org{
		ID:          uuid.New().String(),
		Name:        name,
		Slug:        slug,
		Description: description,
		CreatedAt:   now,
	}

	_, err := d.conn.Exec(
		`INSERT INTO orgs (id, name, slug, description, created_at) VALUES (?, ?, ?, ?, ?)`,
		org.ID, org.Name, org.Slug, org.Description, org.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create org: %w", err)
	}
	return org, nil
}

func (d *DB) ListOrgs() ([]models.Org, error) {
	rows, err := d.ro().Query(`SELECT id, name, slug, description, created_at FROM orgs ORDER BY name LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var orgs []models.Org
	for rows.Next() {
		var o models.Org
		if err := rows.Scan(&o.ID, &o.Name, &o.Slug, &o.Description, &o.CreatedAt); err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

func (d *DB) GetOrg(slug string) (*models.Org, error) {
	var o models.Org
	err := d.ro().QueryRow(
		`SELECT id, name, slug, description, created_at FROM orgs WHERE slug = ?`, slug,
	).Scan(&o.ID, &o.Name, &o.Slug, &o.Description, &o.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org: %w", err)
	}
	return &o, nil
}

// --- Teams ---

func (d *DB) CreateTeam(name, slug, project, description, teamType string, orgID, parentTeamID *string) (*models.Team, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	if teamType == "" {
		teamType = "regular"
	}

	team := &models.Team{
		ID:           uuid.New().String(),
		Name:         name,
		Slug:         slug,
		OrgID:        orgID,
		Project:      project,
		Description:  description,
		Type:         teamType,
		ParentTeamID: parentTeamID,
		CreatedAt:    now,
	}

	_, err := d.conn.Exec(
		`INSERT INTO teams (id, name, slug, org_id, project, description, type, parent_team_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		team.ID, team.Name, team.Slug, team.OrgID, team.Project,
		team.Description, team.Type, team.ParentTeamID, team.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create team: %w", err)
	}
	return team, nil
}

func (d *DB) ListTeams(project string) ([]models.Team, error) {
	rows, err := d.ro().Query(
		`SELECT id, name, slug, org_id, project, description, type, parent_team_id, created_at
		 FROM teams WHERE project = ? ORDER BY name`, project,
	)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var teams []models.Team
	for rows.Next() {
		var t models.Team
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.OrgID, &t.Project,
			&t.Description, &t.Type, &t.ParentTeamID, &t.CreatedAt); err != nil {
			return nil, err
		}
		teams = append(teams, t)
	}
	return teams, rows.Err()
}

func (d *DB) ListAllTeams() ([]models.Team, error) {
	rows, err := d.ro().Query(
		`SELECT id, name, slug, org_id, project, description, type, parent_team_id, created_at
		 FROM teams ORDER BY project, name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var teams []models.Team
	for rows.Next() {
		var t models.Team
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.OrgID, &t.Project,
			&t.Description, &t.Type, &t.ParentTeamID, &t.CreatedAt); err != nil {
			return nil, err
		}
		teams = append(teams, t)
	}
	return teams, rows.Err()
}

func (d *DB) GetTeam(project, slug string) (*models.Team, error) {
	var t models.Team
	err := d.ro().QueryRow(
		`SELECT id, name, slug, org_id, project, description, type, parent_team_id, created_at
		 FROM teams WHERE project = ? AND slug = ?`, project, slug,
	).Scan(&t.ID, &t.Name, &t.Slug, &t.OrgID, &t.Project,
		&t.Description, &t.Type, &t.ParentTeamID, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get team: %w", err)
	}
	return &t, nil
}

func (d *DB) GetTeamByID(teamID string) (*models.Team, error) {
	var t models.Team
	err := d.ro().QueryRow(
		`SELECT id, name, slug, org_id, project, description, type, parent_team_id, created_at
		 FROM teams WHERE id = ?`, teamID,
	).Scan(&t.ID, &t.Name, &t.Slug, &t.OrgID, &t.Project,
		&t.Description, &t.Type, &t.ParentTeamID, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get team by id: %w", err)
	}
	return &t, nil
}

// --- Team Members ---

func (d *DB) AddTeamMember(teamID, agentName, project, role string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	if role == "" {
		role = "member"
	}
	_, err := d.conn.Exec(
		`INSERT OR REPLACE INTO team_members (team_id, agent_name, project, role, joined_at)
		 VALUES (?, ?, ?, ?, ?)`,
		teamID, agentName, project, role, now,
	)
	if err != nil {
		return fmt.Errorf("add team member: %w", err)
	}
	return nil
}

func (d *DB) RemoveTeamMember(teamID, agentName string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec(
		`UPDATE team_members SET left_at = ? WHERE team_id = ? AND agent_name = ? AND left_at IS NULL`,
		now, teamID, agentName,
	)
	if err != nil {
		return fmt.Errorf("remove team member: %w", err)
	}
	return nil
}

func (d *DB) GetTeamMembers(teamID string) ([]models.TeamMember, error) {
	rows, err := d.ro().Query(
		`SELECT team_id, agent_name, project, role, joined_at, left_at
		 FROM team_members WHERE team_id = ? AND left_at IS NULL ORDER BY role, agent_name`, teamID,
	)
	if err != nil {
		return nil, fmt.Errorf("get team members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []models.TeamMember
	for rows.Next() {
		var m models.TeamMember
		if err := rows.Scan(&m.TeamID, &m.AgentName, &m.Project, &m.Role, &m.JoinedAt, &m.LeftAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// GetTeamMemberNames returns active member names for a team.
func (d *DB) GetTeamMemberNames(teamID string) ([]string, error) {
	rows, err := d.ro().Query(
		`SELECT agent_name FROM team_members WHERE team_id = ? AND left_at IS NULL`, teamID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// GetAgentTeams returns teams an agent belongs to.
func (d *DB) GetAgentTeams(project, agentName string) ([]models.Team, error) {
	rows, err := d.ro().Query(
		`SELECT t.id, t.name, t.slug, t.org_id, t.project, t.description, t.type, t.parent_team_id, t.created_at
		 FROM teams t
		 JOIN team_members tm ON t.id = tm.team_id
		 WHERE tm.agent_name = ? AND tm.project = ? AND tm.left_at IS NULL
		 ORDER BY t.name`,
		agentName, project,
	)
	if err != nil {
		return nil, fmt.Errorf("get agent teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var teams []models.Team
	for rows.Next() {
		var t models.Team
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.OrgID, &t.Project,
			&t.Description, &t.Type, &t.ParentTeamID, &t.CreatedAt); err != nil {
			return nil, err
		}
		teams = append(teams, t)
	}
	return teams, rows.Err()
}

// --- Team Inbox ---

func (d *DB) AddToTeamInbox(teamID, messageID string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec(
		`INSERT OR IGNORE INTO team_inbox (team_id, message_id, created_at) VALUES (?, ?, ?)`,
		teamID, messageID, now,
	)
	return err
}

func (d *DB) GetTeamInbox(teamID string, limit int) ([]models.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.ro().Query(
		`SELECT m.id, m.from_agent, m.to_agent, m.reply_to, m.type, m.subject, m.content,
		        m.metadata, m.created_at, m.read_at, m.conversation_id, m.project
		 FROM messages m
		 JOIN team_inbox ti ON m.id = ti.message_id
		 WHERE ti.team_id = ?
		 ORDER BY m.created_at DESC LIMIT ?`,
		teamID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get team inbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []models.Message
	for rows.Next() {
		var m models.Message
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.ReplyTo, &m.Type, &m.Subject, &m.Content,
			&m.Metadata, &m.CreatedAt, &m.ReadAt, &m.ConversationID, &m.Project); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// --- Notify Channels ---

func (d *DB) AddNotifyChannel(agentName, project, target string) error {
	_, err := d.conn.Exec(
		`INSERT OR IGNORE INTO agent_notify_channels (agent_name, project, target) VALUES (?, ?, ?)`,
		agentName, project, target,
	)
	return err
}

func (d *DB) GetNotifyChannels(agentName, project string) ([]string, error) {
	rows, err := d.ro().Query(
		`SELECT target FROM agent_notify_channels WHERE agent_name = ? AND project = ?`,
		agentName, project,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var targets []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

// --- Permission check ---

// CanMessage checks if sender can message target.
// Rules:
//   - Broadcast ("*") → only admin team members
//   - Same team → allowed
//   - reports_to chain → allowed
//   - notify_channels → allowed
//   - Admin team type → unrestricted
func (d *DB) CanMessage(project, sender, target string) (bool, error) {
	// Broadcast: sender must be in an admin-type team
	if target == "*" {
		var count int
		err := d.ro().QueryRow(
			`SELECT COUNT(*) FROM team_members tm
			 JOIN teams t ON tm.team_id = t.id
			 WHERE tm.agent_name = ? AND tm.project = ? AND tm.left_at IS NULL AND t.type = 'admin'`,
			sender, project,
		).Scan(&count)
		if err != nil {
			return false, err
		}
		return count > 0, nil
	}

	// Check if sender is in an admin team (unrestricted)
	var adminCount int
	_ = d.ro().QueryRow(
		`SELECT COUNT(*) FROM team_members tm
		 JOIN teams t ON tm.team_id = t.id
		 WHERE tm.agent_name = ? AND tm.project = ? AND tm.left_at IS NULL AND t.type = 'admin'`,
		sender, project,
	).Scan(&adminCount)
	if adminCount > 0 {
		return true, nil
	}

	// Same team check
	var sameTeam int
	_ = d.ro().QueryRow(
		`SELECT COUNT(*) FROM team_members tm1
		 JOIN team_members tm2 ON tm1.team_id = tm2.team_id
		 WHERE tm1.agent_name = ? AND tm2.agent_name = ? AND tm1.project = ?
		   AND tm1.left_at IS NULL AND tm2.left_at IS NULL`,
		sender, target, project,
	).Scan(&sameTeam)
	if sameTeam > 0 {
		return true, nil
	}

	// reports_to chain check (direct only — sender reports to target or target reports to sender)
	var reportsChain int
	_ = d.ro().QueryRow(
		`SELECT COUNT(*) FROM agents
		 WHERE project = ? AND (
			(name = ? AND reports_to = ?) OR
			(name = ? AND reports_to = ?)
		 )`,
		project, sender, target, target, sender,
	).Scan(&reportsChain)
	if reportsChain > 0 {
		return true, nil
	}

	// Notify channels check
	var notifyCount int
	_ = d.ro().QueryRow(
		`SELECT COUNT(*) FROM agent_notify_channels
		 WHERE agent_name = ? AND project = ? AND target = ?`,
		sender, project, target,
	).Scan(&notifyCount)
	if notifyCount > 0 {
		return true, nil
	}

	// Privilege escalation check: if sender has active elevation to admin, allow
	elevation, err := d.GetActiveElevation(project, sender)
	if err == nil && elevation != nil && elevation.ElevatedRole == "admin" {
		return true, nil
	}

	return false, nil
}

// HasTeams returns true if any teams exist for the project (to skip permission check when no teams are configured).
func (d *DB) HasTeams(project string) (bool, error) {
	var count int
	err := d.ro().QueryRow(`SELECT COUNT(*) FROM teams WHERE project = ?`, project).Scan(&count)
	return count > 0, err
}

// ResolveTeamSlug looks up a team by "team:slug" target and returns the team.
func (d *DB) ResolveTeamSlug(project, slug string) (*models.Team, error) {
	return d.GetTeam(project, slug)
}

// TeamMembershipInfo contains a team ref and the member's role.
type TeamMembershipInfo struct {
	AgentName string
	Project   string
	TeamSlug  string
	TeamName  string
	TeamType  string
	Role      string
}

// GetAllTeamMemberships returns all active team memberships across all projects.
func (d *DB) GetAllTeamMemberships() ([]TeamMembershipInfo, error) {
	rows, err := d.ro().Query(
		`SELECT tm.agent_name, tm.project, t.slug, t.name, t.type, tm.role
		 FROM team_members tm
		 JOIN teams t ON tm.team_id = t.id
		 WHERE tm.left_at IS NULL
		 ORDER BY tm.agent_name, t.name`,
	)
	if err != nil {
		return nil, fmt.Errorf("get all team memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []TeamMembershipInfo
	for rows.Next() {
		var m TeamMembershipInfo
		if err := rows.Scan(&m.AgentName, &m.Project, &m.TeamSlug, &m.TeamName, &m.TeamType, &m.Role); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
