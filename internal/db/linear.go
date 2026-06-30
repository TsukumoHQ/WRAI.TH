package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-relay/internal/models"

	"github.com/google/uuid"
)

// LinearMirrorSeed carries the full Linear zone for a mirror upsert keyed by
// linear_issue_id. It extends LinearTaskSeed with the issue id and the resolved
// parent task id (relay UUID of the parent issue's mirror row, if known). The
// connector resolves the relay row by linear_issue_id before writing so the
// stable relay task id is preserved across updates.
type LinearMirrorSeed struct {
	Project         string
	LinearIssueID   string // Linear issue UUID — the mirror key
	LinearKey       *string
	Title           string
	Description     string
	Priority        string
	Status          string // coarse relay status mapped from the Linear state type
	ExternalURL     *string
	Points          *int
	Labels          string // json array; defaults to "[]"
	LinearState     *string
	Assignee        *string
	LinearProjectID *string // Linear project UUID — persisted; drives project→agent routing
	CycleID         *string
	CycleName       *string
	CycleStart      *string
	CycleEnd        *string
	ParentTaskID    *string // relay task id of the parent issue's mirror row
}

// GetTaskByLinearIssueID returns the mirror row for a Linear issue id, or
// (nil, nil) when none exists yet.
func (d *DB) GetTaskByLinearIssueID(project, linearIssueID string) (*models.Task, error) {
	if linearIssueID == "" {
		return nil, nil
	}
	row := d.ro().QueryRow(
		"SELECT "+taskColumns+" FROM tasks WHERE linear_issue_id = ? AND project = ?",
		linearIssueID, project,
	)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task by linear issue id: %w", err)
	}
	return &t, nil
}

// UpsertLinearMirror writes the Linear zone of a mirror row keyed by
// linear_issue_id. It NEVER touches the relay overlay (claimed_by, in_review_at,
// done_at, blocked_periods) — Linear is SSOT for content only. Returns the
// stable relay task id (created on first sight, preserved afterwards).
//
// Source is forced to 'linear'. On first insert a UUID is minted; on update the
// existing row's id is reused so overlay/temporal state survives.
func (d *DB) UpsertLinearMirror(s LinearMirrorSeed) (taskID string, created bool, err error) {
	if s.LinearIssueID == "" {
		return "", false, fmt.Errorf("upsert linear mirror: empty linear_issue_id")
	}
	if s.Project == "" {
		s.Project = "default"
	}
	if s.Priority == "" {
		s.Priority = "P2"
	}
	if s.Status == "" {
		s.Status = "pending"
	}
	if s.Labels == "" {
		s.Labels = "[]"
	}

	existing, err := d.GetTaskByLinearIssueID(s.Project, s.LinearIssueID)
	if err != nil {
		return "", false, err
	}

	now := time.Now().UTC().Format(memoryTimeFmt)

	if existing != nil {
		// Update the Linear zone only — overlay columns are left untouched.
		_, err = d.conn.Exec(
			`UPDATE tasks SET
			   title=?, description=?, priority=?, status=?, source='linear',
			   linear_key=?, external_url=?, points=?, labels=?, linear_state=?,
			   assignee=?, cycle_id=?, cycle_name=?, cycle_start=?, cycle_end=?,
			   linear_project_id=COALESCE(?, linear_project_id),
			   parent_task_id=COALESCE(?, parent_task_id)
			 WHERE id=? AND project=?`,
			s.Title, s.Description, s.Priority, s.Status,
			s.LinearKey, s.ExternalURL, s.Points, s.Labels, s.LinearState,
			s.Assignee, s.CycleID, s.CycleName, s.CycleStart, s.CycleEnd,
			s.LinearProjectID,
			s.ParentTaskID, existing.ID, s.Project,
		)
		if err != nil {
			return "", false, fmt.Errorf("update linear mirror: %w", err)
		}
		// A Linear-driven status CHANGE is activity — the dispatch (Todo→In
		// Progress) is the OPPOSITE of stale, but the mirror update alone left
		// last_activity_at at created_at, so a freshly dispatched task read as
		// instantly stale. Bump it on any status transition, mirroring the
		// agent-side reset-on-activity (transitionTask).
		if existing.Status != s.Status {
			_, _ = d.conn.Exec("UPDATE tasks SET last_activity_at = ? WHERE id = ?", now, existing.ID)
		}
		return existing.ID, false, nil
	}

	id := uuid.New().String()
	_, err = d.conn.Exec(
		`INSERT INTO tasks
		   (id, profile_slug, dispatched_by, title, description, priority, status, project, dispatched_at,
		    source, linear_issue_id, linear_key, external_url, points, labels, linear_state, assignee,
		    cycle_id, cycle_name, cycle_start, cycle_end, parent_task_id, linear_project_id, last_activity_at, blocked_periods)
		 VALUES (?, '', 'linear', ?, ?, ?, ?, ?, ?, 'linear', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]')`,
		id, s.Title, s.Description, s.Priority, s.Status, s.Project, now,
		s.LinearIssueID, s.LinearKey, s.ExternalURL, s.Points, s.Labels, s.LinearState, s.Assignee,
		s.CycleID, s.CycleName, s.CycleStart, s.CycleEnd, s.ParentTaskID, s.LinearProjectID, now,
	)
	if err != nil {
		return "", false, fmt.Errorf("insert linear mirror: %w", err)
	}
	return id, true, nil
}

// ListBackfillTasks returns native relay tasks eligible for relay→Linear
// backfill: active (not done/cancelled), not archived, not already a Linear
// mirror, and not yet linked to a Linear issue.
func (d *DB) ListBackfillTasks(project string) ([]models.Task, error) {
	rows, err := d.ro().Query(
		"SELECT "+taskColumns+" FROM tasks WHERE project = ? AND archived_at IS NULL "+
			"AND status NOT IN ('done','cancelled') AND COALESCE(source,'native') != 'linear' "+
			"AND (linear_issue_id IS NULL OR linear_issue_id = '') ORDER BY dispatched_at ASC",
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("list backfill tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var tasks []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backfill task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// LinkTaskLinear links an existing native task to a Linear issue (sets the
// mirror key) without otherwise touching it — the next reconcile upsert then
// reconciles content. linear_key is only overwritten when non-empty.
func (d *DB) LinkTaskLinear(taskID, linearIssueID, linearKey string) error {
	if taskID == "" || linearIssueID == "" {
		return fmt.Errorf("link task linear: empty id")
	}
	_, err := d.conn.Exec(
		"UPDATE tasks SET linear_issue_id = ?, linear_key = COALESCE(NULLIF(?, ''), linear_key) WHERE id = ?",
		linearIssueID, linearKey, taskID,
	)
	if err != nil {
		return fmt.Errorf("link task linear: %w", err)
	}
	return nil
}

// MarkLinearDone stamps the overlay's done_at / completed_at when a Done webhook
// echoes back (the one inbound exception that touches the overlay — Linear owns
// Done via the GitHub PR-merge auto-close). Idempotent: only stamps if unset.
func (d *DB) MarkLinearDone(taskID string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec(
		`UPDATE tasks SET
		   done_at = COALESCE(done_at, ?),
		   completed_at = COALESCE(completed_at, ?)
		 WHERE id = ?`,
		now, now, taskID,
	)
	if err != nil {
		return fmt.Errorf("mark linear done: %w", err)
	}
	return nil
}

// LinearMirrorRef is a minimal handle on a Linear-sourced mirror task.
type LinearMirrorRef struct {
	TaskID        string
	LinearIssueID string
}

// ActiveLinearMirrors lists the project's Linear-sourced mirror tasks that are
// still active (not done/cancelled) and carry an issue id — the candidate set
// for the reconcile Done-dropout sync (TSU-159).
func (d *DB) ActiveLinearMirrors(project string) ([]LinearMirrorRef, error) {
	rows, err := d.ro().Query(
		`SELECT id, linear_issue_id FROM tasks
		 WHERE project = ? AND source = 'linear'
		   AND linear_issue_id IS NOT NULL AND linear_issue_id != ''
		   AND status NOT IN ('done', 'cancelled')`,
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("active linear mirrors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LinearMirrorRef
	for rows.Next() {
		var r LinearMirrorRef
		if err := rows.Scan(&r.TaskID, &r.LinearIssueID); err != nil {
			return nil, fmt.Errorf("scan linear mirror: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CloseLinearMirror terminally closes a mirror task to match a Linear issue that
// finished while out of the open poll (TSU-159). status must be "done" or
// "cancelled". Guarded so it never re-opens or flips an already-terminal task,
// and stamps the terminal timestamps the stale-scanner / boards read.
func (d *DB) CloseLinearMirror(taskID, status string) error {
	if status != "done" && status != "cancelled" {
		return fmt.Errorf("close linear mirror: invalid status %q", status)
	}
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec(
		`UPDATE tasks SET
		   status = ?,
		   done_at = COALESCE(done_at, ?),
		   completed_at = COALESCE(completed_at, ?),
		   last_activity_at = ?
		 WHERE id = ? AND status NOT IN ('done', 'cancelled')`,
		status, now, now, now, taskID,
	)
	if err != nil {
		return fmt.Errorf("close linear mirror: %w", err)
	}
	return nil
}

// linearSyncLogCap bounds the audit table; older rows are pruned on insert.
const linearSyncLogCap = 500

// LogLinearSync appends a write-back outcome to the capped audit table.
// Best-effort: a logging failure never blocks the connector.
func (d *DB) LogLinearSync(issueID, action, outcome, detail string) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	if _, err := d.conn.Exec(
		`INSERT INTO linear_sync_log (ts, issue_id, action, outcome, detail) VALUES (?, ?, ?, ?, ?)`,
		now, issueID, action, outcome, detail,
	); err != nil {
		return
	}
	// Prune beyond the cap (keep the newest linearSyncLogCap rows).
	_, _ = d.conn.Exec(
		`DELETE FROM linear_sync_log WHERE id <= (
		   SELECT id FROM linear_sync_log ORDER BY id DESC LIMIT 1 OFFSET ?
		 )`,
		linearSyncLogCap,
	)
}

// LinearSyncEntry is one row of the write-back audit trail.
type LinearSyncEntry struct {
	TS      string `json:"ts"`
	IssueID string `json:"issue_id"`
	Action  string `json:"action"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// RecentLinearSync returns the newest write-back log entries (newest first).
func (d *DB) RecentLinearSync(limit int) ([]LinearSyncEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.ro().Query(
		`SELECT ts, issue_id, action, outcome, detail FROM linear_sync_log ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("recent linear sync: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LinearSyncEntry
	for rows.Next() {
		var e LinearSyncEntry
		if err := rows.Scan(&e.TS, &e.IssueID, &e.Action, &e.Outcome, &e.Detail); err != nil {
			return nil, fmt.Errorf("scan linear sync: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
