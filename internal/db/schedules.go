package db

import (
	"time"
)

// UpsertSchedule creates or updates a schedule.
func (d *DB) UpsertSchedule(id, agentName, project, name, cronExpr, prompt, ttl, cycle, allowedTools string) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = d.conn.Exec(`INSERT INTO schedules (id, agent_name, project, name, cron_expr, prompt, ttl, cycle, allowed_tools, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET cron_expr = ?, prompt = ?, ttl = ?, cycle = ?, allowed_tools = ?, enabled = 1, updated_at = ?`,
		id, agentName, project, name, cronExpr, prompt, ttl, cycle, allowedTools, now, now,
		cronExpr, prompt, ttl, cycle, allowedTools, now)
}

// DeleteSchedule removes a schedule.
func (d *DB) DeleteSchedule(id string) {
	_, _ = d.conn.Exec(`DELETE FROM schedules WHERE id = ?`, id)
}

// SetScheduleEnabled flips the enabled flag on a schedule.
func (d *DB) SetScheduleEnabled(id string, enabled bool) {
	now := time.Now().UTC().Format(time.RFC3339)
	e := 0
	if enabled {
		e = 1
	}
	_, _ = d.conn.Exec(`UPDATE schedules SET enabled = ?, updated_at = ? WHERE id = ?`, e, now, id)
}

// GetSchedule returns a single schedule by ID.
func (d *DB) GetSchedule(id string) map[string]any {
	var agentName, project, name, cronExpr, prompt, ttl, cycle, allowedTools, createdAt, updatedAt string
	var enabled int
	err := d.ro().QueryRow(`SELECT agent_name, project, name, cron_expr, prompt, ttl, cycle, allowed_tools, enabled, created_at, updated_at
		FROM schedules WHERE id = ?`, id).Scan(&agentName, &project, &name, &cronExpr, &prompt, &ttl, &cycle, &allowedTools, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return nil
	}
	return map[string]any{
		"id":            id,
		"agent_name":    agentName,
		"project":       project,
		"name":          name,
		"cron_expr":     cronExpr,
		"prompt":        prompt,
		"ttl":           ttl,
		"cycle":         cycle,
		"allowed_tools": allowedTools,
		"enabled":       enabled == 1,
		"created_at":    createdAt,
		"updated_at":    updatedAt,
	}
}

// ListSchedulesByAgent returns schedules for an agent in a project.
func (d *DB) ListSchedulesByAgent(project, agentName string) []map[string]any {
	rows, err := d.ro().Query(`SELECT id, agent_name, project, name, cron_expr, prompt, ttl, cycle, allowed_tools, enabled, created_at, updated_at
		FROM schedules WHERE project = ? AND agent_name = ? ORDER BY name LIMIT 200`, project, agentName)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanScheduleRows(rows)
}

// ListSchedulesByProject returns all schedules for a project.
func (d *DB) ListSchedulesByProject(project string) []map[string]any {
	rows, err := d.ro().Query(`SELECT id, agent_name, project, name, cron_expr, prompt, ttl, cycle, allowed_tools, enabled, created_at, updated_at
		FROM schedules WHERE project = ? ORDER BY agent_name, name LIMIT 500`, project)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanScheduleRows(rows)
}

// ListAllEnabledSchedules returns all enabled schedules across all projects.
func (d *DB) ListAllEnabledSchedules() []map[string]any {
	rows, err := d.ro().Query(`SELECT id, agent_name, project, name, cron_expr, prompt, ttl, cycle, allowed_tools, enabled, created_at, updated_at
		FROM schedules WHERE enabled = 1 ORDER BY project, agent_name, name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanScheduleRows(rows)
}

func scanScheduleRows(rows interface {
	Next() bool
	Scan(...any) error
}) []map[string]any {
	var result []map[string]any
	for rows.Next() {
		var id, agentName, project, name, cronExpr, prompt, ttl, cycle, allowedTools, createdAt, updatedAt string
		var enabled int
		if err := rows.Scan(&id, &agentName, &project, &name, &cronExpr, &prompt, &ttl, &cycle, &allowedTools, &enabled, &createdAt, &updatedAt); err != nil {
			continue
		}
		result = append(result, map[string]any{
			"id":            id,
			"agent_name":    agentName,
			"project":       project,
			"name":          name,
			"cron_expr":     cronExpr,
			"prompt":        prompt,
			"ttl":           ttl,
			"cycle":         cycle,
			"allowed_tools": allowedTools,
			"enabled":       enabled == 1,
			"created_at":    createdAt,
			"updated_at":    updatedAt,
		})
	}
	return result
}
