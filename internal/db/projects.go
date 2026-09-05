package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// canonicalProject is the DB-layer canonical form of a project identifier:
// TRIM + LOWER + underscores folded to hyphens. It MUST stay in lockstep with
// relay.NormalizeProject (the handler-boundary normalizer) and with the SQL
// form baked into migrateNormalizeProjects — the three are one rule expressed
// three ways. Applying it inside every project-keyed helper here makes the
// projects registry case/underscore-insensitive at the single choke point all
// callers pass through, so a path that reaches the DB without going through a
// handler (or a handler that forgot to normalize) still can't create a
// case-split duplicate or lose a lookup. Empty and internal "_"-prefixed
// pseudo-projects are left untouched, matching the migration's exemption.
func canonicalProject(name string) string {
	if name == "" || strings.HasPrefix(name, "_") {
		return name
	}
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
}

// Planet pool: category/variant pairs (48x48, 60 frames each).
var planetPool = []string{
	"barren/1", "barren/2", "barren/3", "barren/4",
	"desert/1", "desert/2",
	"forest/1", "forest/2",
	"gas_giant/1", "gas_giant/2", "gas_giant/3", "gas_giant/4",
	"ice/1",
	"lava/1", "lava/2", "lava/3",
	"ocean/1",
	"terran/1", "terran/2",
	"tundra/1", "tundra/2",
}

func randomPlanet() string {
	return planetPool[rand.Intn(len(planetPool))]
}

// EnsureProject creates a project entry if it doesn't exist, assigning a random planet.
func (d *DB) EnsureProject(name string) {
	name = canonicalProject(name)
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = d.writerExec(
		"INSERT OR IGNORE INTO projects (name, planet_type, created_at) VALUES (?, ?, ?)",
		name, randomPlanet(), now,
	)
}

// GetProject returns a project by name.
func (d *DB) GetProject(name string) (*models.Project, error) {
	name = canonicalProject(name)
	var p models.Project
	err := d.ro().QueryRow("SELECT name, planet_type, created_at FROM projects WHERE name = ?", name).Scan(&p.Name, &p.PlanetType, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateProjectPlanetType changes a project's planet_type.
func (d *DB) UpdateProjectPlanetType(name, planetType string) error {
	name = canonicalProject(name)
	_, err := d.writerExec("UPDATE projects SET planet_type = ? WHERE name = ?", planetType, name)
	return err
}

// ProjectRequiresTypedTicket reports whether `name` enforces typed tickets at
// dispatch (goal + acceptance criteria + dod mandatory). Default off — an
// unknown project or a missing column reads as false, so the many free-form
// projects the relay serves keep dispatching untouched. niwa is seeded on.
func (d *DB) ProjectRequiresTypedTicket(name string) bool {
	name = canonicalProject(name)
	var v int
	err := d.ro().QueryRow("SELECT require_typed_ticket FROM projects WHERE name = ?", name).Scan(&v)
	if err != nil {
		return false
	}
	return v != 0
}

// SetProjectRequiresTypedTicket flips the per-project enforcement flag. The row
// must already exist (projects are created on first agent registration).
func (d *DB) SetProjectRequiresTypedTicket(name string, required bool) error {
	name = canonicalProject(name)
	v := 0
	if required {
		v = 1
	}
	_, err := d.writerExec("UPDATE projects SET require_typed_ticket = ? WHERE name = ?", v, name)
	return err
}

// ProjectRequiresMemoryDiscipline reports whether `name` enforces Memory
// Protocol v2's write-side guard (DEC-governance-memory-protocol-2): no
// checkpoint/resume/dated keys, layer=context requires a bounded valid_until,
// non-empty tags, and remember() rejects area="general". Default off (same
// opt-in + debrayable philosophy as typed-ticket, DEC-governance-enforcement-1)
// — an unknown project or a missing column reads as false. niwa+tsukumo seeded on.
func (d *DB) ProjectRequiresMemoryDiscipline(name string) bool {
	name = canonicalProject(name)
	var v int
	err := d.ro().QueryRow("SELECT require_memory_discipline FROM projects WHERE name = ?", name).Scan(&v)
	if err != nil {
		return false
	}
	return v != 0
}

// SetProjectRequiresMemoryDiscipline flips the per-project enforcement flag.
// The row must already exist (projects are created on first agent registration).
func (d *DB) SetProjectRequiresMemoryDiscipline(name string, required bool) error {
	name = canonicalProject(name)
	v := 0
	if required {
		v = 1
	}
	_, err := d.writerExec("UPDATE projects SET require_memory_discipline = ? WHERE name = ?", v, name)
	return err
}

// GetSetting returns a setting value by key.
func (d *DB) GetSetting(key string) string {
	var val string
	_ = d.ro().QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&val)
	return val
}

// SetSetting upserts a setting.
func (d *DB) SetSetting(key, value string) {
	_, _ = d.writerExec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?", key, value, value)
}

// DeleteProject removes a project and all its associated data (cascade delete).
func (d *DB) DeleteProject(name string) error {
	name = canonicalProject(name)
	tx, err := d.beginWriterTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// NOTE: PRAGMA foreign_keys is a no-op inside an active transaction (SQLite
	// ignores it once BEGIN has run), so we don't try to disable FK enforcement.
	// Instead the deletes below are ordered children-first (junction tables →
	// project-scoped tables → orgs → the project row) so every FK constraint is
	// already satisfied at each step. Keep this ordering when adding tables.

	// Delete junction tables that lack a project column (linked via IDs)
	_, _ = tx.Exec("DELETE FROM conversation_members WHERE conversation_id IN (SELECT id FROM conversations WHERE project = ?)", name)
	_, _ = tx.Exec("DELETE FROM conversation_reads WHERE conversation_id IN (SELECT id FROM conversations WHERE project = ?)", name)
	_, _ = tx.Exec("DELETE FROM team_inbox WHERE team_id IN (SELECT id FROM teams WHERE project = ?)", name)
	_, _ = tx.Exec("DELETE FROM message_reads WHERE message_id IN (SELECT id FROM messages WHERE project = ?)", name)

	// Delete all related data (tables with a project column)
	tables := []string{
		"token_usage", "deliveries", "agent_notify_channels", "team_members", "teams",
		"boards", "vault_docs", "vaults", "file_locks",
		"memories", "profiles", "tasks", "conversations", "messages", "agents",
	}
	for _, t := range tables {
		if _, err := tx.Exec("DELETE FROM "+t+" WHERE project = ?", name); err != nil {
			return fmt.Errorf("delete from %s: %w", t, err)
		}
	}

	// Delete orgs that no longer have any teams (orgs lack a project column;
	// they are linked indirectly via teams.org_id).
	if _, err := tx.Exec(`DELETE FROM orgs WHERE id NOT IN (SELECT DISTINCT org_id FROM teams WHERE org_id IS NOT NULL)`); err != nil {
		return fmt.Errorf("delete orphan orgs: %w", err)
	}

	// Delete the project itself
	res, err := tx.Exec("DELETE FROM projects WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %q not found", name)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// isLinearBackedProject reports whether name is a mirror target in
// linear_project_map — a VALUE in the {linearProjectId: relayProject} JSON.
// Archiving such a project is fail-closed until S7 (design §5): the
// reconcile/backfill loop would keep writing to a frozen project. name is
// expected already canonicalized by the caller.
func (d *DB) isLinearBackedProject(name string) bool {
	raw := d.GetSetting("linear_project_map")
	if raw == "" {
		return false
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return false
	}
	for _, relayProj := range m {
		if canonicalProject(relayProj) == name {
			return true
		}
	}
	return false
}

// ArchiveProject soft-archives a project: a reversible status flip with ZERO
// cascade (DEC-wraith-archive-project-1). No other table is touched — agents,
// tasks, messages, memories, deliveries all persist, frozen. Already-archived or
// unknown = explicit error (refuse loudly). Fail-closed on Linear-backed
// projects until S7.
func (d *DB) ArchiveProject(name string) error {
	name = canonicalProject(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	if d.isLinearBackedProject(name) {
		return fmt.Errorf("project %q is Linear-backed (in linear_project_map); unlink it from Linear before archiving", name)
	}
	res, err := d.writerExec(
		"UPDATE projects SET archived_at = ? WHERE name = ? AND archived_at IS NULL",
		time.Now().UTC().Format(time.RFC3339), name)
	if err != nil {
		return fmt.Errorf("archive project: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish not-found from already-archived for a loud, actionable error.
		var archivedAt sql.NullString
		switch err := d.ro().QueryRow("SELECT archived_at FROM projects WHERE name = ?", name).Scan(&archivedAt); err {
		case sql.ErrNoRows:
			return fmt.Errorf("project %q not found", name)
		case nil:
			return fmt.Errorf("project %q is already archived", name)
		default:
			return fmt.Errorf("archive project: %w", err)
		}
	}
	return nil
}

// UnarchiveProject reverses ArchiveProject with zero data loss (nothing was ever
// deleted). Unknown or not-archived = explicit error.
func (d *DB) UnarchiveProject(name string) error {
	name = canonicalProject(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	res, err := d.writerExec(
		"UPDATE projects SET archived_at = NULL WHERE name = ? AND archived_at IS NOT NULL", name)
	if err != nil {
		return fmt.Errorf("unarchive project: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var cnt int
		_ = d.ro().QueryRow("SELECT COUNT(*) FROM projects WHERE name = ?", name).Scan(&cnt)
		if cnt == 0 {
			return fmt.Errorf("project %q not found", name)
		}
		return fmt.Errorf("project %q is not archived", name)
	}
	return nil
}

// IsProjectArchived reports whether a project exists and is archived
// (archived_at IS NOT NULL). A missing project or a read error returns false
// (fail-open — the caller's existence/identity checks own unknown projects).
// Read-only; the S3b archived-project freeze guard's single source of truth.
func (d *DB) IsProjectArchived(name string) bool {
	name = canonicalProject(name)
	if name == "" {
		return false
	}
	var archivedAt sql.NullString
	if err := d.ro().QueryRow(
		"SELECT archived_at FROM projects WHERE name = ?", name).Scan(&archivedAt); err != nil {
		return false
	}
	return archivedAt.Valid && archivedAt.String != ""
}

// ListProjectsWithInfo returns ACTIVE projects with their planet_type and stats.
// Archived projects are excluded (DEC-wraith-archive-project-1); use
// ListProjectsWithInfoFiltered(true) to include them.
func (d *DB) ListProjectsWithInfo() ([]models.ProjectInfo, error) {
	return d.ListProjectsWithInfoFiltered(false)
}

// ListProjectsWithInfoFiltered returns projects with stats. When includeArchived
// is false, archived projects (projects.archived_at IS NOT NULL) are excluded;
// when true they are returned with archived_at populated.
func (d *DB) ListProjectsWithInfoFiltered(includeArchived bool) ([]models.ProjectInfo, error) {
	since24h := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	where := ""
	if !includeArchived {
		where = "WHERE p.archived_at IS NULL"
	}
	rows, err := d.ro().Query(`
		SELECT p.name, p.planet_type, p.created_at,
			COALESCE(ac.agent_count, 0),
			COALESCE(ac.online_count, 0),
			COALESCE(tc.total_tasks, 0),
			COALESCE(tc.active_tasks, 0),
			COALESCE(tc.done_tasks, 0),
			COALESCE(tc.blocked_tasks, 0),
			COALESCE(tu.tokens_24h, 0),
			COALESCE(tc.last_activity, ''),
			COALESCE(p.archived_at, '')
		FROM projects p
		LEFT JOIN (
			SELECT project, COUNT(*) as agent_count,
				SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END) as online_count
			FROM agents WHERE status IN ('active', 'sleeping', 'inactive')
			GROUP BY project
		) ac ON ac.project = p.name
		LEFT JOIN (
			SELECT project, COUNT(*) as total_tasks,
				SUM(CASE WHEN status IN ('accepted', 'in-progress', 'in-review') THEN 1 ELSE 0 END) as active_tasks,
				SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END) as done_tasks,
				SUM(CASE WHEN status = 'blocked' THEN 1 ELSE 0 END) as blocked_tasks,
				MAX(COALESCE(completed_at, started_at, accepted_at, dispatched_at)) as last_activity
			FROM tasks GROUP BY project
		) tc ON tc.project = p.name
		LEFT JOIN (
			SELECT project, SUM(bytes)/4 as tokens_24h
			FROM token_usage WHERE created_at >= ? GROUP BY project
		) tu ON tu.project = p.name
		`+where+`
		ORDER BY p.name
	`, since24h)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var projects []models.ProjectInfo
	for rows.Next() {
		var p models.ProjectInfo
		if err := rows.Scan(&p.Name, &p.PlanetType, &p.CreatedAt, &p.AgentCount, &p.OnlineCount, &p.TotalTasks, &p.ActiveTasks, &p.DoneTasks, &p.BlockedTasks, &p.Tokens24h, &p.LastActivity, &p.ArchivedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}
