package db

import (
	"agent-relay/internal/models"
	"agent-relay/internal/normalize"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// blockedPeriod is one {start,end} window in the auto-stamped blocked_periods trail.
type blockedPeriod struct {
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
}

// openBlockedPeriod appends a new open {start: now} window to the existing JSON array.
func openBlockedPeriod(existing, now string) string {
	var periods []blockedPeriod
	if existing != "" {
		_ = json.Unmarshal([]byte(existing), &periods)
	}
	periods = append(periods, blockedPeriod{Start: now})
	b, _ := json.Marshal(periods)
	return string(b)
}

// closeBlockedPeriod sets end=now on the last open window. No-op if none is open.
func closeBlockedPeriod(existing, now string) string {
	var periods []blockedPeriod
	if existing != "" {
		_ = json.Unmarshal([]byte(existing), &periods)
	}
	for i := len(periods) - 1; i >= 0; i-- {
		if periods[i].End == "" {
			periods[i].End = now
			break
		}
	}
	if periods == nil {
		return "[]"
	}
	b, _ := json.Marshal(periods)
	return string(b)
}

// Valid task state transitions
// "done" and "cancelled" are reachable from any state (flexible cleanup)
// "in-review" sits between in-progress and done (the agent's "PR up" signal).
var validTransitions = map[string][]string{
	"pending":     {"accepted", "in-progress", "done", "cancelled"},
	"accepted":    {"in-progress", "done", "cancelled"},
	"in-progress": {"in-review", "done", "blocked", "cancelled"},
	"in-review":   {"in-progress", "done", "blocked", "cancelled"},
	"blocked":     {"in-progress", "in-review", "done", "cancelled"},
	"done":        {"cancelled"},
	"cancelled":   {},
}

const taskColumns = "id, profile_slug, assigned_to, dispatched_by, title, description, priority, status, result, blocked_reason, project, dispatched_at, accepted_at, started_at, completed_at, parent_task_id, ack_notified_at, ack_escalated_at, board_id, archived_at, " +
	"source, linear_issue_id, linear_key, external_url, points, labels, linear_state, assignee, cycle_id, cycle_name, cycle_start, cycle_end, " +
	"claimed_by, claimed_at, blocked_periods, in_review_at, done_at"

func scanTask(row interface{ Scan(...any) error }) (models.Task, error) {
	var t models.Task
	err := row.Scan(&t.ID, &t.ProfileSlug, &t.AssignedTo, &t.DispatchedBy, &t.Title, &t.Description,
		&t.Priority, &t.Status, &t.Result, &t.BlockedReason, &t.Project,
		&t.DispatchedAt, &t.AcceptedAt, &t.StartedAt, &t.CompletedAt, &t.ParentTaskID,
		&t.AckNotifiedAt, &t.AckEscalatedAt, &t.BoardID, &t.ArchivedAt,
		&t.Source, &t.LinearIssueID, &t.LinearKey, &t.ExternalURL, &t.Points, &t.Labels,
		&t.LinearState, &t.Assignee, &t.CycleID, &t.CycleName, &t.CycleStart, &t.CycleEnd,
		&t.ClaimedBy, &t.ClaimedAt, &t.BlockedPeriods, &t.InReviewAt, &t.DoneAt)
	return t, err
}

func (d *DB) DispatchTask(project, profileSlug, dispatchedBy, title, description, priority string, parentTaskID, boardID *string) (*models.Task, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	if priority == "" {
		priority = "P2"
	}

	task := &models.Task{
		ID:             uuid.New().String(),
		ProfileSlug:    profileSlug,
		DispatchedBy:   dispatchedBy,
		Title:          title,
		Description:    description,
		Priority:       priority,
		Status:         "pending",
		Project:        project,
		DispatchedAt:   now,
		ParentTaskID:   parentTaskID,
		BoardID:        boardID,
		Source:         "native",
		Labels:         "[]",
		BlockedPeriods: "[]",
	}

	_, err := d.conn.Exec(
		`INSERT INTO tasks (id, profile_slug, dispatched_by, title, description, priority, status, project, dispatched_at, parent_task_id, board_id, source)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'native')`,
		task.ID, task.ProfileSlug, task.DispatchedBy, task.Title, task.Description,
		task.Priority, task.Status, task.Project, task.DispatchedAt, task.ParentTaskID, task.BoardID,
	)
	if err != nil {
		return nil, fmt.Errorf("dispatch task: %w", err)
	}
	return task, nil
}

// ReviewTask transitions a task to in-review (the agent's "PR up" signal).
func (d *DB) ReviewTask(taskID, agentName, project string) (*models.Task, error) {
	return d.transitionTask(taskID, agentName, project, "in-review", nil, nil)
}

func (d *DB) ResetTask(taskID, agentName, project string) (*models.Task, error) {
	return d.transitionTask(taskID, agentName, project, "pending", nil, nil)
}

func (d *DB) ClaimTask(taskID, agentName, project string) (*models.Task, error) {
	return d.transitionTask(taskID, agentName, project, "accepted", nil, nil)
}

func (d *DB) StartTask(taskID, agentName, project string) (*models.Task, error) {
	return d.transitionTask(taskID, agentName, project, "in-progress", nil, nil)
}

func (d *DB) CompleteTask(taskID, agentName, project string, result *string) (*models.Task, error) {
	return d.transitionTask(taskID, agentName, project, "done", result, nil)
}

func (d *DB) BlockTask(taskID, agentName, project string, reason *string) (*models.Task, error) {
	return d.transitionTask(taskID, agentName, project, "blocked", nil, reason)
}

func (d *DB) CancelTask(taskID, agentName, project string, reason *string) (*models.Task, error) {
	return d.transitionTask(taskID, agentName, project, "cancelled", nil, reason)
}

func (d *DB) transitionTask(taskID, agentName, project, newStatus string, result, blockedReason *string) (*models.Task, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)

	task, err := d.GetTask(taskID, project)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	// Validate transition (skip for user — admin can force any move)
	if agentName != "user" {
		allowed := validTransitions[task.Status]
		valid := false
		for _, s := range allowed {
			if s == newStatus {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf("invalid transition: %s → %s", task.Status, newStatus)
		}
	}

	// Was the task blocked before this transition? If so, any transition OUT of
	// blocked closes the open blocked window in the auto-stamped trail.
	leavingBlocked := task.Status == "blocked" && newStatus != "blocked"

	// Build update. Every transition auto-stamps its temporal trail with zero
	// manual input.
	task.Status = newStatus
	switch newStatus {
	case "pending":
		task.AssignedTo = nil
		task.AcceptedAt = nil
		task.StartedAt = nil
		task.CompletedAt = nil
		task.Result = nil
		task.BlockedReason = nil
		task.ClaimedBy = nil
		task.ClaimedAt = nil
		task.InReviewAt = nil
		task.DoneAt = nil
		task.BlockedPeriods = "[]"
		_, err = d.conn.Exec(
			"UPDATE tasks SET status = ?, assigned_to = NULL, accepted_at = NULL, started_at = NULL, completed_at = NULL, result = NULL, blocked_reason = NULL, claimed_by = NULL, claimed_at = NULL, in_review_at = NULL, done_at = NULL, blocked_periods = '[]' WHERE id = ? AND project = ?",
			newStatus, taskID, project,
		)
	case "accepted":
		// claim → claimed_at + claimed_by (also sets assigned_to + accepted_at)
		task.AssignedTo = &agentName
		task.AcceptedAt = &now
		task.ClaimedBy = &agentName
		task.ClaimedAt = &now
		_, err = d.conn.Exec(
			"UPDATE tasks SET status = ?, assigned_to = ?, accepted_at = ?, claimed_by = ?, claimed_at = ? WHERE id = ? AND project = ?",
			newStatus, agentName, now, agentName, now, taskID, project,
		)
	case "in-progress":
		// start → started_at (and close any open blocked window on resume)
		task.AssignedTo = &agentName
		task.StartedAt = &now
		if leavingBlocked {
			task.BlockedPeriods = closeBlockedPeriod(task.BlockedPeriods, now)
			_, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, assigned_to = ?, started_at = ?, blocked_periods = ? WHERE id = ? AND project = ?",
				newStatus, agentName, now, task.BlockedPeriods, taskID, project,
			)
		} else {
			_, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, assigned_to = ?, started_at = ? WHERE id = ? AND project = ?",
				newStatus, agentName, now, taskID, project,
			)
		}
	case "in-review":
		// in-review → in_review_at (close any open blocked window if resuming via review)
		task.InReviewAt = &now
		if task.AssignedTo == nil {
			task.AssignedTo = &agentName
		}
		if leavingBlocked {
			task.BlockedPeriods = closeBlockedPeriod(task.BlockedPeriods, now)
			_, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, assigned_to = COALESCE(assigned_to, ?), in_review_at = ?, blocked_periods = ? WHERE id = ? AND project = ?",
				newStatus, agentName, now, task.BlockedPeriods, taskID, project,
			)
		} else {
			_, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, assigned_to = COALESCE(assigned_to, ?), in_review_at = ? WHERE id = ? AND project = ?",
				newStatus, agentName, now, taskID, project,
			)
		}
	case "done":
		// done → done_at (alias of completed_at, stamped together)
		task.CompletedAt = &now
		task.DoneAt = &now
		result = normalizePtr(result)
		task.Result = result
		if leavingBlocked {
			task.BlockedPeriods = closeBlockedPeriod(task.BlockedPeriods, now)
			_, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, result = ?, completed_at = ?, done_at = ?, blocked_periods = ? WHERE id = ? AND project = ?",
				newStatus, result, now, now, task.BlockedPeriods, taskID, project,
			)
		} else {
			_, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, result = ?, completed_at = ?, done_at = ? WHERE id = ? AND project = ?",
				newStatus, result, now, now, taskID, project,
			)
		}
	case "blocked":
		// block → append {start: now} to blocked_periods
		task.BlockedReason = blockedReason
		task.BlockedPeriods = openBlockedPeriod(task.BlockedPeriods, now)
		_, err = d.conn.Exec(
			"UPDATE tasks SET status = ?, blocked_reason = ?, blocked_periods = ? WHERE id = ? AND project = ?",
			newStatus, blockedReason, task.BlockedPeriods, taskID, project,
		)
	case "cancelled":
		task.CompletedAt = &now
		task.BlockedReason = blockedReason // reuse as cancellation reason
		if leavingBlocked {
			task.BlockedPeriods = closeBlockedPeriod(task.BlockedPeriods, now)
			_, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, blocked_reason = ?, completed_at = ?, blocked_periods = ? WHERE id = ? AND project = ?",
				newStatus, blockedReason, now, task.BlockedPeriods, taskID, project,
			)
		} else {
			_, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, blocked_reason = ?, completed_at = ? WHERE id = ? AND project = ?",
				newStatus, blockedReason, now, taskID, project,
			)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("update task status: %w", err)
	}
	return task, nil
}

func (d *DB) GetTask(taskID, project string) (*models.Task, error) {
	t, err := scanTask(d.ro().QueryRow(
		"SELECT "+taskColumns+" FROM tasks WHERE id = ? AND project = ?",
		taskID, project,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return &t, nil
}

// GetTaskWithSubtasks returns a task with its subtask chain (max depth 3).
func (d *DB) GetTaskWithSubtasks(taskID, project string) (*models.Task, error) {
	task, err := d.GetTask(taskID, project)
	if err != nil || task == nil {
		return task, err
	}
	task.Subtasks, _ = d.getSubtasks(taskID, project, 0, 3)
	return task, nil
}

func (d *DB) getSubtasks(parentID, project string, depth, maxDepth int) ([]models.Task, error) {
	if depth >= maxDepth {
		return nil, nil
	}
	rows, err := d.ro().Query(
		"SELECT "+taskColumns+" FROM tasks WHERE parent_task_id = ? AND project = ? ORDER BY dispatched_at",
		parentID, project,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// Collect all tasks first to close rows before recursive calls
	var tasks []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()

	// Now recursively fetch subtasks (rows is closed, no deadlock)
	for i := range tasks {
		tasks[i].Subtasks, _ = d.getSubtasks(tasks[i].ID, project, depth+1, maxDepth)
	}
	return tasks, nil
}

// GetAgentTasks returns tasks assigned to or dispatched by an agent (for session_context).
// All three queries are LIMITed to keep session_context bounded (paper Def. 7).
// dispatched_by_me is explicitly filtered to active statuses only — cancelled,
// done, and failed tasks would otherwise inflate the payload past the MCP output
// limit for agents with long dispatch history.
func (d *DB) GetAgentTasks(project, agentName string) (assignedToMe []models.Task, dispatchedByMe []models.Task, err error) {
	// Assigned to me (active tasks) — close rows before next query
	assignedToMe, err = d.queryTasks(
		"SELECT "+taskColumns+" FROM tasks WHERE assigned_to = ? AND project = ? AND archived_at IS NULL AND status IN ('pending','accepted','in-progress') ORDER BY CASE priority WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2 WHEN 'P3' THEN 3 END LIMIT 50",
		agentName, project,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("get assigned tasks: %w", err)
	}

	// Also get pending tasks for my profile
	pending, err := d.queryTasks(
		`SELECT `+taskColumns+` FROM tasks WHERE project = ? AND archived_at IS NULL AND status = 'pending' AND assigned_to IS NULL
		 AND profile_slug IN (SELECT profile_slug FROM agents WHERE name = ? AND project = ? AND profile_slug IS NOT NULL)
		 ORDER BY CASE priority WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2 WHEN 'P3' THEN 3 END LIMIT 50`,
		project, agentName, project,
	)
	if err == nil {
		assignedToMe = append(assignedToMe, pending...)
	}

	// Dispatched by me — active statuses only (pending/accepted/in-progress/blocked).
	// Historical cancelled/done/failed tasks are reachable via list_tasks on demand.
	dispatchedByMe, err = d.queryTasks(
		"SELECT "+taskColumns+" FROM tasks WHERE dispatched_by = ? AND project = ? AND archived_at IS NULL AND status IN ('pending','accepted','in-progress','blocked') ORDER BY CASE priority WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2 WHEN 'P3' THEN 3 END, dispatched_at DESC LIMIT 20",
		agentName, project,
	)
	if err != nil {
		return assignedToMe, nil, fmt.Errorf("get dispatched tasks: %w", err)
	}

	return assignedToMe, dispatchedByMe, nil
}

// GetOldestPendingTaskForProfile returns the oldest pending task for a profile
// in a project. Used to re-fire task.dispatched after a child completes and the
// pool frees up.
func (d *DB) GetOldestPendingTaskForProfile(project, profileSlug string) (*models.Task, error) {
	row := d.ro().QueryRow(
		"SELECT "+taskColumns+" FROM tasks WHERE project = ? AND profile_slug = ? AND status = 'pending' AND archived_at IS NULL ORDER BY dispatched_at ASC LIMIT 1",
		project, profileSlug,
	)
	t, err := scanTask(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get oldest pending task: %w", err)
	}
	return &t, nil
}

// queryTasks runs a query and collects all tasks, closing rows before returning.
func (d *DB) queryTasks(query string, args ...any) ([]models.Task, error) {
	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tasks []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// GetUnackedTasks returns pending tasks older than minAge that haven't been notified yet.
func (d *DB) GetUnackedTasks(minAge time.Duration) ([]models.Task, error) {
	cutoff := time.Now().UTC().Add(-minAge).Format(memoryTimeFmt)
	rows, err := d.ro().Query(
		"SELECT "+taskColumns+" FROM tasks WHERE status = 'pending' AND archived_at IS NULL AND dispatched_at < ?",
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("get unacked tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// MarkTaskAckNotified sets the ack_notified_at timestamp.
func (d *DB) MarkTaskAckNotified(taskID string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec("UPDATE tasks SET ack_notified_at = ? WHERE id = ?", now, taskID)
	return err
}

// MarkTaskAckEscalated sets the ack_escalated_at timestamp.
func (d *DB) MarkTaskAckEscalated(taskID string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec("UPDATE tasks SET ack_escalated_at = ? WHERE id = ?", now, taskID)
	return err
}

// GetParentChain walks up the parent_task_id chain (max depth 5).
func (d *DB) GetParentChain(taskID, project string) ([]models.Task, error) {
	var chain []models.Task
	currentID := taskID
	for i := 0; i < 5; i++ {
		var parentID *string
		err := d.ro().QueryRow("SELECT parent_task_id FROM tasks WHERE id = ? AND project = ?", currentID, project).Scan(&parentID)
		if err != nil || parentID == nil {
			break
		}
		parent, err := d.GetTask(*parentID, project)
		if err != nil || parent == nil {
			break
		}
		chain = append(chain, *parent)
		currentID = *parentID
	}
	return chain, nil
}

func (d *DB) ListTasks(project, status, profileSlug, priority, assignedTo, boardID string, limit int, includeArchived bool) ([]models.Task, error) {
	if limit <= 0 {
		limit = 50
	}

	query := "SELECT " + taskColumns + " FROM tasks WHERE project = ?"
	args := []any{project}

	if !includeArchived {
		query += " AND archived_at IS NULL"
	}

	if status == "active" {
		query += " AND status NOT IN ('done', 'cancelled')"
	} else if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	if profileSlug != "" {
		query += " AND profile_slug = ?"
		args = append(args, profileSlug)
	}
	if priority != "" {
		query += " AND priority = ?"
		args = append(args, priority)
	}
	if assignedTo != "" {
		query += " AND assigned_to = ?"
		args = append(args, assignedTo)
	}
	if boardID != "" {
		query += " AND board_id = ?"
		args = append(args, boardID)
	}

	query += " ORDER BY CASE priority WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2 WHEN 'P3' THEN 3 END, dispatched_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (d *DB) ListAllTasks(limit int) ([]models.Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.ro().Query(
		"SELECT "+taskColumns+" FROM tasks WHERE archived_at IS NULL ORDER BY CASE priority WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2 WHEN 'P3' THEN 3 END, dispatched_at DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list all tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (d *DB) UpdateTaskFields(taskID, project string, title, description, priority, boardID *string) (*models.Task, error) {
	task, err := d.GetTask(taskID, project)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	if title != nil {
		task.Title = *title
	}
	if description != nil {
		task.Description = *description
	}
	if priority != nil {
		task.Priority = *priority
	}
	if boardID != nil {
		task.BoardID = boardID
	}

	_, err = d.conn.Exec(
		"UPDATE tasks SET title = ?, description = ?, priority = ?, board_id = ? WHERE id = ? AND project = ?",
		task.Title, task.Description, task.Priority, task.BoardID, taskID, project,
	)
	if err != nil {
		return nil, fmt.Errorf("update task: %w", err)
	}
	return task, nil
}

func (d *DB) DeleteTask(taskID, project string) error {
	_, err := d.conn.Exec("DELETE FROM tasks WHERE id = ? AND project = ?", taskID, project)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	return nil
}

// FindSimilarTasks checks for existing non-done/cancelled tasks with a similar title under the same profile.
func (d *DB) FindSimilarTasks(project, profileSlug, title string) ([]models.Task, error) {
	// Use LIKE with the first 20 chars of the title for a rough match
	search := title
	if len(search) > 20 {
		search = search[:20]
	}
	return d.queryTasks(
		"SELECT "+taskColumns+" FROM tasks WHERE project = ? AND profile_slug = ? AND status NOT IN ('done','cancelled') AND title LIKE ? LIMIT 5",
		project, profileSlug, "%"+search+"%",
	)
}

// CheckSubtasksComplete checks if all subtasks of a parent task are done or cancelled.
// Returns (allComplete, total, doneCount).
func (d *DB) CheckSubtasksComplete(parentTaskID, project string) (bool, int, int) {
	var total, doneCount int
	_ = d.ro().QueryRow(
		"SELECT COUNT(*) FROM tasks WHERE parent_task_id = ? AND project = ?",
		parentTaskID, project,
	).Scan(&total)
	if total == 0 {
		return false, 0, 0
	}
	_ = d.ro().QueryRow(
		"SELECT COUNT(*) FROM tasks WHERE parent_task_id = ? AND project = ? AND status IN ('done','cancelled')",
		parentTaskID, project,
	).Scan(&doneCount)
	return doneCount >= total, total, doneCount
}

func (d *DB) GetTasksSince(project, since string, limit int) ([]models.Task, error) {
	if limit <= 0 {
		limit = 100
	}
	query := "SELECT " + taskColumns + " FROM tasks WHERE archived_at IS NULL AND (dispatched_at > ? OR accepted_at > ? OR started_at > ? OR completed_at > ?)"
	args := []any{since, since, since, since}
	if project != "" {
		query += " AND project = ?"
		args = append(args, project)
	}
	query += " ORDER BY dispatched_at ASC LIMIT ?"
	args = append(args, limit)

	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get tasks since: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// ArchiveTasks soft-deletes tasks matching the given filters.
// status: "done", "cancelled", or "" for both done+cancelled. boardID: filter by board, or "" for all.
func (d *DB) ArchiveTasks(project, status, boardID string) (int64, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)

	query := "UPDATE tasks SET archived_at = ? WHERE project = ? AND archived_at IS NULL"
	args := []any{now, project}

	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	} else {
		query += " AND status IN ('done', 'cancelled')"
	}

	if boardID != "" {
		query += " AND board_id = ?"
		args = append(args, boardID)
	}

	result, err := d.conn.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("archive tasks: %w", err)
	}
	return result.RowsAffected()
}

// LinearTaskSeed carries the Linear-zone fields for upserting a mirror task.
// All pointer fields are optional. Used to populate the read-replica from the
// Linear connector (and by tests to exercise the cycle/board endpoints).
type LinearTaskSeed struct {
	ID           string
	Project      string
	Title        string
	Description  string
	Priority     string
	Status       string // native status the board maps from when linear_state is unset
	LinearKey    *string
	ExternalURL  *string
	Points       *int
	Labels       string // json array; defaults to "[]"
	LinearState  *string
	Assignee     *string
	CycleID      *string
	CycleName    *string
	CycleStart   *string
	CycleEnd     *string
	DispatchedAt string
}

// UpsertLinearTask inserts or replaces a mirror task carrying the Linear zone.
// Source is forced to 'linear'. This is the read-replica write primitive: the
// relay never authors these from the UI — they originate from Linear via the
// connector (or tests).
func (d *DB) UpsertLinearTask(s LinearTaskSeed) error {
	if s.ID == "" {
		s.ID = uuid.New().String()
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
	if s.DispatchedAt == "" {
		s.DispatchedAt = time.Now().UTC().Format(memoryTimeFmt)
	}
	_, err := d.conn.Exec(
		`INSERT INTO tasks
		   (id, profile_slug, dispatched_by, title, description, priority, status, project, dispatched_at,
		    source, linear_key, external_url, points, labels, linear_state, assignee,
		    cycle_id, cycle_name, cycle_start, cycle_end, blocked_periods)
		 VALUES (?, '', 'linear', ?, ?, ?, ?, ?, ?, 'linear', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]')
		 ON CONFLICT(id) DO UPDATE SET
		   title=excluded.title, description=excluded.description, priority=excluded.priority,
		   status=excluded.status, linear_key=excluded.linear_key, external_url=excluded.external_url,
		   points=excluded.points, labels=excluded.labels, linear_state=excluded.linear_state,
		   assignee=excluded.assignee, cycle_id=excluded.cycle_id, cycle_name=excluded.cycle_name,
		   cycle_start=excluded.cycle_start, cycle_end=excluded.cycle_end`,
		s.ID, s.Title, s.Description, s.Priority, s.Status, s.Project, s.DispatchedAt,
		s.LinearKey, s.ExternalURL, s.Points, s.Labels, s.LinearState, s.Assignee,
		s.CycleID, s.CycleName, s.CycleStart, s.CycleEnd,
	)
	if err != nil {
		return fmt.Errorf("upsert linear task: %w", err)
	}
	return nil
}

// Cycle is one Linear cycle (sprint) the mirror knows about, used by the kanban
// cycle filter. Active is true when today falls within [Start, End].
type Cycle struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Start  string `json:"start,omitempty"`
	End    string `json:"end,omitempty"`
	Active bool   `json:"active"`
	Count  int    `json:"count"` // number of (non-archived) tasks in the cycle
}

// ListCycles returns the distinct cycles present in the mirror for a project,
// newest start first. The cycle whose [start,end] window spans today is marked
// active. Native-only projects (no Linear cycles) return an empty slice.
func (d *DB) ListCycles(project string) ([]Cycle, error) {
	rows, err := d.ro().Query(
		`SELECT cycle_id, COALESCE(cycle_name, ''), COALESCE(cycle_start, ''), COALESCE(cycle_end, ''), COUNT(*)
		 FROM tasks
		 WHERE project = ? AND archived_at IS NULL AND cycle_id IS NOT NULL AND cycle_id != ''
		 GROUP BY cycle_id, cycle_name, cycle_start, cycle_end
		 ORDER BY cycle_start DESC`,
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("list cycles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	now := time.Now().UTC().Format("2006-01-02")
	var cycles []Cycle
	for rows.Next() {
		var c Cycle
		if err := rows.Scan(&c.ID, &c.Name, &c.Start, &c.End, &c.Count); err != nil {
			return nil, fmt.Errorf("scan cycle: %w", err)
		}
		c.Active = cycleSpansDate(c.Start, c.End, now)
		cycles = append(cycles, c)
	}
	return cycles, rows.Err()
}

// cycleSpansDate reports whether day (YYYY-MM-DD) falls within [start, end].
// Timestamps are compared on their date prefix so RFC3339 and date-only values
// both work. An empty bound is treated as open on that side.
func cycleSpansDate(start, end, day string) bool {
	if start == "" && end == "" {
		return false
	}
	startOK := start == "" || datePrefix(start) <= day
	endOK := end == "" || day <= datePrefix(end)
	return startOK && endOK
}

func datePrefix(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// ListBoardTasks returns all non-archived, non-cancelled tasks for the kanban
// board in one query (no priority-only LIMIT truncation). When cycleID is
// non-empty, only tasks in that cycle are returned; "all" or "" returns every
// task. Tasks are returned flat (the board nests by parent_task_id client-side).
// Ordering is priority → points → dispatched_at so the board's within-column
// order is correct before any client grouping.
func (d *DB) ListBoardTasks(project, cycleID string, limit int) ([]models.Task, error) {
	if limit <= 0 {
		limit = 1000
	}
	query := "SELECT " + taskColumns + " FROM tasks WHERE project = ? AND archived_at IS NULL AND status != 'cancelled'"
	args := []any{project}
	if cycleID != "" && cycleID != "all" {
		query += " AND cycle_id = ?"
		args = append(args, cycleID)
	}
	query += " ORDER BY CASE priority WHEN 'P0' THEN 0 WHEN 'P1' THEN 1 WHEN 'P2' THEN 2 WHEN 'P3' THEN 3 ELSE 9 END, " +
		"COALESCE(points, 0) DESC, dispatched_at ASC LIMIT ?"
	args = append(args, limit)
	return d.queryTasks(query, args...)
}

// ResolveTaskID resolves a short task ID prefix to a full UUID.
// Returns the full ID if exactly one match is found, or the original if it's already a full UUID.
func (d *DB) ResolveTaskID(prefix, project string) (string, error) {
	// If it looks like a full UUID (36 chars), skip prefix search
	if len(prefix) >= 36 {
		return prefix, nil
	}
	var ids []string
	rows, err := d.ro().Query("SELECT id FROM tasks WHERE id LIKE ? AND project = ?", prefix+"%", project)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return prefix, nil // let downstream report "not found"
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("ambiguous task ID prefix %q (%d matches)", prefix, len(ids))
	}
	return ids[0], nil
}

func normalizePtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := normalize.JSONKeys(*s)
	return &v
}

/* ============================================================= *
 *  Command layer — dependencies & reassignment
 * ============================================================= */

// errLinearReadOnly guards orchestrator mutations against Linear-mirrored tasks:
// Linear is the source of truth for those, so reassignment/force are refused.
var errLinearReadOnly = fmt.Errorf("task is mirrored from Linear (read-only here — Linear is the source of truth)")

// ReassignTask hands a task to a different agent without changing its status —
// the orchestrator's "you take this now" lever. Stamps assigned_to + claimed_by.
func (d *DB) ReassignTask(taskID, project, agent string) (*models.Task, error) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return nil, fmt.Errorf("agent is required")
	}
	task, err := d.GetTask(taskID, project)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	if task.Source == "linear" {
		return nil, errLinearReadOnly
	}
	if _, err = d.conn.Exec(
		"UPDATE tasks SET assigned_to = ?, claimed_by = ? WHERE id = ? AND project = ?",
		agent, agent, taskID, project,
	); err != nil {
		return nil, fmt.Errorf("reassign task: %w", err)
	}
	task.AssignedTo = &agent
	task.ClaimedBy = &agent
	return task, nil
}
