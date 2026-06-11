package db

import (
	"time"
)

// InsertSpawnChild records a spawned child in the database.
func (d *DB) InsertSpawnChild(id, parentAgent, project, profile, prompt string) {
	_, _ = d.conn.Exec(`INSERT INTO spawn_children (id, parent_agent, project, profile, prompt, status, started_at)
		VALUES (?, ?, ?, ?, ?, 'running', ?)`,
		id, parentAgent, project, profile, prompt, time.Now().UTC().Format(time.RFC3339))
}

// UpdateSpawnChild updates a child's status after completion. Stdout and stderr
// tails are truncated to tailMaxStdout / tailMaxStderr bytes before persisting.
// The full assembled prompt is cleared on completion — it averages ~88KB/row
// and was 86% of relay.db size. The row is preserved for audit; the prompt is
// reconstructible from (profile, cycle, task) and lives in /private/tmp/relay.log
// for same-day debug.
func (d *DB) UpdateSpawnChild(id, status string, exitCode int, errMsg, stdoutTail, stderrTail string) {
	_, _ = d.conn.Exec(
		`UPDATE spawn_children SET status = ?, exit_code = ?, error = ?, finished_at = ?, stdout_tail = ?, stderr_tail = ?, prompt = '' WHERE id = ?`,
		status, exitCode, errMsg, time.Now().UTC().Format(time.RFC3339),
		tailBytes(stdoutTail, tailMaxStdout), tailBytes(stderrTail, tailMaxStderr), id,
	)
}

// PurgeSpawnChildren deletes finished/killed/failed spawn rows older than maxAge.
// Running children are always preserved. Returns the number of rows removed.
func (d *DB) PurgeSpawnChildren(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
	res, err := d.conn.Exec(
		`DELETE FROM spawn_children
		 WHERE status IN ('finished','killed','failed')
		   AND finished_at IS NOT NULL AND finished_at < ?`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

const (
	tailMaxStdout = 2048
	tailMaxStderr = 4096
)

func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ListSpawnChildren returns children for a parent agent, optionally filtered by status.
func (d *DB) ListSpawnChildren(parentAgent, project, status string) []map[string]any {
	query := `SELECT id, parent_agent, project, profile, status, started_at, finished_at, exit_code, error
		FROM spawn_children WHERE project = ?`
	args := []any{project}

	if parentAgent != "" {
		query += " AND parent_agent = ?"
		args = append(args, parentAgent)
	}

	if status != "" && status != "all" {
		query += " AND status = ?"
		args = append(args, status)
	}
	query += " ORDER BY started_at DESC LIMIT 50"

	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id, parent, proj, prof, st, startedAt string
		var finishedAt, errMsg *string
		var exitCode *int
		if err := rows.Scan(&id, &parent, &proj, &prof, &st, &startedAt, &finishedAt, &exitCode, &errMsg); err != nil {
			continue
		}
		m := map[string]any{
			"id":           id,
			"parent_agent": parent,
			"project":      proj,
			"profile":      prof,
			"status":       st,
			"started_at":   startedAt,
		}
		if finishedAt != nil {
			m["finished_at"] = *finishedAt
		}
		if exitCode != nil {
			m["exit_code"] = *exitCode
		}
		if errMsg != nil && *errMsg != "" {
			m["error"] = *errMsg
		}
		result = append(result, m)
	}
	return result
}

// GetSpawnChild returns a single child by ID.
func (d *DB) GetSpawnChild(id string) map[string]any {
	var parent, project, profile, status, startedAt string
	var finishedAt, errMsg *string
	var exitCode *int
	err := d.ro().QueryRow(`SELECT parent_agent, project, profile, status, started_at, finished_at, exit_code, error
		FROM spawn_children WHERE id = ?`, id).Scan(&parent, &project, &profile, &status, &startedAt, &finishedAt, &exitCode, &errMsg)
	if err != nil {
		return nil
	}
	m := map[string]any{
		"id":           id,
		"parent_agent": parent,
		"project":      project,
		"profile":      profile,
		"status":       status,
		"started_at":   startedAt,
	}
	if finishedAt != nil {
		m["finished_at"] = *finishedAt
	}
	if exitCode != nil {
		m["exit_code"] = *exitCode
	}
	if errMsg != nil && *errMsg != "" {
		m["error"] = *errMsg
	}
	return m
}

// ListRunningChildren returns all children with status "running" across all agents/projects.
func (d *DB) ListRunningChildren() []map[string]any {
	rows, err := d.ro().Query(`SELECT id, parent_agent, project, profile, status, started_at, finished_at, exit_code, error
		FROM spawn_children WHERE status = 'running' ORDER BY started_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id, parent, proj, prof, st, startedAt string
		var finishedAt, errMsg *string
		var exitCode *int
		if err := rows.Scan(&id, &parent, &proj, &prof, &st, &startedAt, &finishedAt, &exitCode, &errMsg); err != nil {
			continue
		}
		result = append(result, map[string]any{
			"id":           id,
			"parent_agent": parent,
			"project":      proj,
			"profile":      prof,
			"status":       st,
			"started_at":   startedAt,
		})
	}
	return result
}

// GetActiveSpawnCount returns the number of running children for a project.
func (d *DB) GetActiveSpawnCount(project string) int {
	var count int
	_ = d.ro().QueryRow(`SELECT COUNT(*) FROM spawn_children WHERE project = ? AND status = 'running'`, project).Scan(&count)
	return count
}
