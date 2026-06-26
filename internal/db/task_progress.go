package db

import (
	"fmt"
	"time"
)

// ProgressNote is an ad-hoc status update attached to a task between claim and
// complete. Surfaced in the web UI activity feed and task detail panel.
type ProgressNote struct {
	ID        int64  `json:"id"`
	TaskID    string `json:"task_id"`
	Project   string `json:"project"`
	Agent     string `json:"agent"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
}

// AddProgressNote appends a progress note for a task.
func (d *DB) AddProgressNote(taskID, project, agent, note string) error {
	if note == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.conn.Exec(
		`INSERT INTO task_progress_notes (task_id, project, agent, note, created_at) VALUES (?, ?, ?, ?, ?)`,
		taskID, project, agent, note, now,
	)
	if err != nil {
		return fmt.Errorf("add progress note: %w", err)
	}
	// A comment / progress note is activity — reset the stale clock.
	_, _ = d.conn.Exec("UPDATE tasks SET last_activity_at = ? WHERE id = ? AND project = ?", now, taskID, project)
	return nil
}

// GetProgressNotes returns notes for a task in chronological order.
func (d *DB) GetProgressNotes(taskID, project string) ([]ProgressNote, error) {
	rows, err := d.ro().Query(
		`SELECT id, task_id, project, agent, note, created_at
		 FROM task_progress_notes WHERE task_id = ? AND project = ? ORDER BY created_at ASC LIMIT 200`,
		taskID, project,
	)
	if err != nil {
		return nil, fmt.Errorf("get progress notes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var notes []ProgressNote
	for rows.Next() {
		var n ProgressNote
		if err := rows.Scan(&n.ID, &n.TaskID, &n.Project, &n.Agent, &n.Note, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan progress note: %w", err)
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}
