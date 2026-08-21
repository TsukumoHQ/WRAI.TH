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
	// backlog = groomed but NOT yet claimable. It reaches only pending (promote)
	// or cancelled — never accepted/in-progress directly, so a claim (→accepted)
	// on a backlog task fails validation and the task cannot be picked up until
	// promoted. pending can be sent back to backlog (de-groom).
	"backlog":     {"pending", "cancelled"},
	"pending":     {"accepted", "in-progress", "done", "cancelled", "backlog"},
	"accepted":    {"in-progress", "done", "cancelled"},
	"in-progress": {"in-review", "done", "blocked", "cancelled"},
	"in-review":   {"in-progress", "done", "blocked", "cancelled"},
	"blocked":     {"in-progress", "in-review", "done", "cancelled"},
	"done":        {"cancelled"},
	"cancelled":   {},
}

const taskColumns = "id, profile_slug, assigned_to, dispatched_by, title, description, priority, status, result, blocked_reason, project, dispatched_at, accepted_at, started_at, completed_at, parent_task_id, ack_notified_at, ack_escalated_at, board_id, archived_at, " +
	"source, linear_issue_id, linear_key, external_url, points, labels, linear_state, assignee, cycle_id, cycle_name, cycle_start, cycle_end, " +
	"claimed_by, claimed_at, blocked_periods, in_review_at, done_at, linear_project_id, last_activity_at, " +
	"git_branch, git_worktree, git_target, " +
	"pr_url, pr_number, pr_state, pr_repo, " +
	"integration_branch, run_state, " +
	"goal, acceptance_criteria, dod, refusal_notified_at, " +
	"lease_holder, lease_expires_at, lease_heartbeat_at"

func scanTask(row interface{ Scan(...any) error }) (models.Task, error) {
	var t models.Task
	err := row.Scan(&t.ID, &t.ProfileSlug, &t.AssignedTo, &t.DispatchedBy, &t.Title, &t.Description,
		&t.Priority, &t.Status, &t.Result, &t.BlockedReason, &t.Project,
		&t.DispatchedAt, &t.AcceptedAt, &t.StartedAt, &t.CompletedAt, &t.ParentTaskID,
		&t.AckNotifiedAt, &t.AckEscalatedAt, &t.BoardID, &t.ArchivedAt,
		&t.Source, &t.LinearIssueID, &t.LinearKey, &t.ExternalURL, &t.Points, &t.Labels,
		&t.LinearState, &t.Assignee, &t.CycleID, &t.CycleName, &t.CycleStart, &t.CycleEnd,
		&t.ClaimedBy, &t.ClaimedAt, &t.BlockedPeriods, &t.InReviewAt, &t.DoneAt, &t.LinearProjectID, &t.LastActivityAt,
		&t.GitBranch, &t.GitWorktree, &t.GitTarget,
		&t.PRURL, &t.PRNumber, &t.PRState, &t.PRRepo,
		&t.IntegrationBranch, &t.RunState,
		&t.Goal, &t.AcceptanceCriteria, &t.Dod, &t.RefusalNotifiedAt,
		&t.LeaseHolder, &t.LeaseExpiresAt, &t.LeaseHeartbeatAt)
	return t, err
}

// TypedTicket carries the V-lifecycle dispatch-time fields. Zero value = no
// ticket recorded (free-form dispatch). The DB layer persists it verbatim; it
// also ENFORCES completeness for projects that opt in (require_typed_ticket) —
// see DispatchTask. Enforcement lives here, at the single creation choke, so no
// caller path (dispatch_task, batch, HTTP API, cron, inbound-signal webhook,
// agent self-dispatch/followup) can create a bare ticket on an enforced project.
type TypedTicket struct {
	Goal               string // one-line intent
	AcceptanceCriteria string // json array of testable items ("" normalised to "[]")
	Dod                string // definition of done
}

// hasAcceptanceItems reports whether raw is a JSON array carrying ≥1 non-blank
// item. An empty string, non-array JSON, or an array of only-blank strings all
// count as "no acceptance criteria".
func hasAcceptanceItems(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var items []string
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return false
	}
	for _, it := range items {
		if strings.TrimSpace(it) != "" {
			return true
		}
	}
	return false
}

// Missing returns which of goal / acceptance_criteria / dod are absent from the
// ticket, in dispatch order. Empty result = a complete typed ticket. This is the
// single source of truth for "what makes a ticket bare" — every enforcement site
// funnels through it, so the definition cannot drift across paths.
func (t TypedTicket) Missing() []string {
	var missing []string
	if strings.TrimSpace(t.Goal) == "" {
		missing = append(missing, "goal")
	}
	if !hasAcceptanceItems(t.AcceptanceCriteria) {
		missing = append(missing, "acceptance_criteria")
	}
	if strings.TrimSpace(t.Dod) == "" {
		missing = append(missing, "dod")
	}
	return missing
}

// TypedTicketError is returned by DispatchTask when an enforced project receives
// a dispatch missing one or more required typed-ticket fields. Callers surface it
// as a refusal (a batch skips the item and reports it; a single dispatch returns
// the message). errors.As unwraps it so a handler can format field-precisely.
type TypedTicketError struct {
	Project string
	Missing []string
}

func (e *TypedTicketError) Error() string {
	return fmt.Sprintf("typed ticket required for project '%s': missing [%s]. "+
		"Dispatch with goal (intent), acceptance_criteria (JSON array of individually testable items) and dod (definition of done).",
		e.Project, strings.Join(e.Missing, ", "))
}

func (d *DB) DispatchTask(project, profileSlug, dispatchedBy, title, description, priority string, parentTaskID, boardID *string, ticket TypedTicket, backlog bool) (*models.Task, error) {
	// Single typed-ticket guard. Every creation path funnels through DispatchTask,
	// so enforcing here (before any write or side-effect) makes a bare ticket
	// impossible on an enforced project — no per-path check to drift or bypass.
	// Checked on the RAW ticket, before acceptance_criteria is normalised to "[]".
	if d.ProjectRequiresTypedTicket(project) {
		if missing := ticket.Missing(); len(missing) > 0 {
			return nil, &TypedTicketError{Project: project, Missing: missing}
		}
	}

	now := time.Now().UTC().Format(memoryTimeFmt)
	if priority == "" {
		priority = "P2"
	}
	acceptanceCriteria := ticket.AcceptanceCriteria
	if strings.TrimSpace(acceptanceCriteria) == "" {
		acceptanceCriteria = "[]"
	}

	// Groomed work can be born straight into 'backlog' (visible, not claimable)
	// so grooming doesn't require a follow-up move; promote_task lifts it to pending.
	status := "pending"
	if backlog {
		status = "backlog"
	}

	task := &models.Task{
		ID:                 uuid.New().String(),
		ProfileSlug:        profileSlug,
		DispatchedBy:       dispatchedBy,
		Title:              title,
		Description:        description,
		Priority:           priority,
		Status:             status,
		Project:            project,
		DispatchedAt:       now,
		ParentTaskID:       parentTaskID,
		BoardID:            boardID,
		Source:             "native",
		Labels:             "[]",
		BlockedPeriods:     "[]",
		Goal:               ticket.Goal,
		AcceptanceCriteria: acceptanceCriteria,
		Dod:                ticket.Dod,
	}

	_, err := d.conn.Exec(
		`INSERT INTO tasks (id, profile_slug, dispatched_by, title, description, priority, status, project, dispatched_at, parent_task_id, board_id, source, last_activity_at, goal, acceptance_criteria, dod)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'native', ?, ?, ?, ?)`,
		task.ID, task.ProfileSlug, task.DispatchedBy, task.Title, task.Description,
		task.Priority, task.Status, task.Project, task.DispatchedAt, task.ParentTaskID, task.BoardID, task.DispatchedAt,
		task.Goal, task.AcceptanceCriteria, task.Dod,
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

// SetTaskGit records where the task's work physically lives (branch /
// worktree / target). Nil values leave the existing column untouched, so a
// resubmission that only names the branch doesn't wipe the worktree recorded
// earlier. The relay stores these opaquely — the review gate consumes them.
func (d *DB) SetTaskGit(taskID, project string, branch, worktree, target *string) error {
	_, err := d.conn.Exec(
		`UPDATE tasks SET
			git_branch = COALESCE(?, git_branch),
			git_worktree = COALESCE(?, git_worktree),
			git_target = COALESCE(?, git_target)
		 WHERE id = ? AND project = ?`,
		branch, worktree, target, taskID, project,
	)
	return err
}

// SetTaskPR records the linked GitHub PR (PR-link S1). Nil args leave the
// existing column untouched (COALESCE), so a status-only sync that knows just
// the new pr_state doesn't wipe the url/number/repo recorded at link time, and
// re-linking is safe. Returns whether the task existed so the caller can report
// NOT_FOUND rather than silently no-op. Additive, single UPDATE (writer tx).
func (d *DB) SetTaskPR(taskID, project string, prURL *string, prNumber *int, prState, prRepo *string) (bool, error) {
	res, err := d.conn.Exec(
		`UPDATE tasks SET
			pr_url = COALESCE(?, pr_url),
			pr_number = COALESCE(?, pr_number),
			pr_state = COALESCE(?, pr_state),
			pr_repo = COALESCE(?, pr_repo)
		 WHERE id = ? AND project = ?`,
		prURL, prNumber, prState, prRepo, taskID, project,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetTaskByPR resolves a task by its linked GitHub PR (number + repo) within a
// project (PR-link S2 resolver). Returns nil (no error) when nothing matches.
func (d *DB) GetTaskByPR(project string, prNumber int, repo string) (*models.Task, error) {
	row := d.ro().QueryRow(
		"SELECT "+taskColumns+" FROM tasks WHERE project = ? AND pr_number = ? AND pr_repo = ? AND archived_at IS NULL LIMIT 1",
		project, prNumber, repo,
	)
	t, err := scanTask(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListPRReconcileCandidates returns tasks with a linked, still-open PR whose
// task is non-terminal — the set an EXTERNAL poller (niwa, which owns gh) GETs
// to catch a missed pull_request webhook and converge (PR-link S3-relay). The
// relay stays INBOUND-only for GitHub (no outbound client / token); it only
// exposes WHO to reconcile, never reaches out itself. Bounded to keep the read
// small.
func (d *DB) ListPRReconcileCandidates(project string, limit int) ([]models.Task, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.ro().Query(
		"SELECT "+taskColumns+" FROM tasks "+
			"WHERE project = ? AND archived_at IS NULL "+
			"AND pr_number IS NOT NULL AND pr_state = 'open' "+
			"AND status NOT IN ('done','cancelled') "+
			"ORDER BY last_activity_at DESC LIMIT ?",
		project, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListStrandedPRTasks returns tasks whose LINKED PR has already reached a
// terminal state (pr_state 'merged' or 'closed', persisted by a received
// pull_request webhook or a poll write-back) but whose TASK is still non-terminal
// — the stranded set the internal reconcile sweep converges (merged→done,
// closed-unmerged→blocked). This is the gap ListPRReconcileCandidates (pr_state
// 'open' only) cannot see: once a webhook flips pr_state to 'merged' but the task
// transition is missed, the task drops out of the open-only candidate set and
// strands in-review forever. The sweep converges off the ALREADY-PERSISTED
// pr_state, no gh call — the relay stays inbound-only. Global across projects (the
// sweep is project-agnostic maintenance). Already-settled rows (closed→blocked) are
// excluded so the set does not grow unbounded; merged→done settles by leaving the
// non-terminal filter. Bounded to keep the read small.
func (d *DB) ListStrandedPRTasks(limit int) ([]models.Task, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := d.ro().Query(
		"SELECT "+taskColumns+" FROM tasks "+
			"WHERE archived_at IS NULL AND pr_number IS NOT NULL "+
			"AND pr_state IN ('merged','closed') "+
			"AND status NOT IN ('done','cancelled') "+
			"AND NOT (pr_state = 'closed' AND status = 'blocked') "+
			"ORDER BY last_activity_at DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []models.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ForcePRTransition applies a PR-driven state change as the supervisor, with a
// NO-RESURRECT guard: a terminal task (done/cancelled) is never moved back by a
// late/duplicate webhook, and an already-in-target task is a no-op. Runs the
// transition on the "user" (force) path so a PR event can move a task along an
// edge the normal agent lifecycle wouldn't (e.g. pending → in-review on PR
// opened). Returns (task, changed, err). PR-link S2.
func (d *DB) ForcePRTransition(project, taskID, target string, reason *string) (*models.Task, bool, error) {
	task, err := d.GetTask(taskID, project)
	if err != nil {
		return nil, false, err
	}
	if task == nil {
		return nil, false, nil
	}
	// No-resurrect: a terminal task stays terminal (ignore a stale opened/sync).
	if task.Status == "done" || task.Status == "cancelled" {
		if target != task.Status {
			return task, false, nil
		}
	}
	if task.Status == target {
		return task, false, nil // idempotent
	}
	updated, err := d.transitionTask(taskID, "user", project, target, nil, reason)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

func (d *DB) ResetTask(taskID, agentName, project string) (*models.Task, error) {
	return d.transitionTask(taskID, agentName, project, "pending", nil, nil)
}

func (d *DB) ClaimTask(taskID, agentName, project string) (*models.Task, error) {
	if err := d.guardNotRunContainer(taskID, project); err != nil {
		return nil, err
	}
	return d.transitionTask(taskID, agentName, project, "accepted", nil, nil)
}

// PromoteTask lifts a groomed 'backlog' task to 'pending' (claimable). It is
// lifecycle-enforced: transitionTask only allows backlog→pending (or, harmlessly,
// pending→pending as an idempotent no-op via the same edge), and refuses any
// other origin with an invalid-transition error.
// PromoteTask returns (task, changed, error): changed is true only when it
// actually moved a backlog task to pending. Promoting an already-pending task is
// a harmless no-op with changed=false so the caller does NOT re-announce (a
// double-promote must not duplicate the fleet wake / task.dispatched / P0 push).
func (d *DB) PromoteTask(taskID, agentName, project string) (*models.Task, bool, error) {
	task, err := d.GetTask(taskID, project)
	if err != nil {
		return nil, false, err
	}
	if task == nil {
		return nil, false, fmt.Errorf("task not found: %s", taskID)
	}
	// Already claimable — no-op (also avoids an invalid pending→pending transition;
	// transitionTask has no same-status edge).
	if task.Status == "pending" {
		return task, false, nil
	}
	updated, err := d.transitionTask(taskID, agentName, project, "pending", nil, nil)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

func (d *DB) StartTask(taskID, agentName, project string) (*models.Task, error) {
	if err := d.guardNotRunContainer(taskID, project); err != nil {
		return nil, err
	}
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
			// A claim (→accepted) on a task that has already left 'pending' is not
			// a malformed request — it is a LOST claim race (a concurrent claimer,
			// or a reclaim, already took it). Surface the typed TASK_STATE_CONFLICT
			// so the caller parks instead of hot-looping on a bare-string error.
			// This makes every double-claim loser get the typed code regardless of
			// whether it read the pre- or post-commit state.
			if newStatus == "accepted" && task.Status == "backlog" {
				return nil, newTaskError(CodeTaskStateConflict,
					"task %s is in backlog (groomed, not yet active); promote it to pending before claiming", taskID)
			}
			if newStatus == "accepted" && task.Status != "pending" {
				return nil, newTaskError(CodeTaskStateConflict,
					"task %s already claimed (status %q) before claim by %q could apply", taskID, task.Status, agentName)
			}
			return nil, fmt.Errorf("invalid transition: %s → %s", task.Status, newStatus)
		}
	}

	// Was the task blocked before this transition? If so, any transition OUT of
	// blocked closes the open blocked window in the auto-stamped trail.
	leavingBlocked := task.Status == "blocked" && newStatus != "blocked"

	// oldStatus is the value we validated against; every UPDATE below carries a
	// compare-and-swap guard (AND status = oldStatus) so the write only lands if
	// the row hasn't changed since GetTask read it.
	oldStatus := task.Status
	// priorHolder is the lease holder before this transition — captured so a
	// releasing transition (done/blocked/cancelled/pending) can report who gave
	// the lease up (task.lease_transferred, reason voluntary).
	priorHolder := strVal(task.LeaseHolder)

	// Build update. Every transition auto-stamps its temporal trail with zero
	// manual input.
	task.Status = newStatus
	var res sql.Result
	switch newStatus {
	case "backlog":
		// De-groom: a task sent to backlog is pre-active, so clear any claim/lease
		// state exactly like pending. (A direct-to-backlog dispatch never has any.)
		task.AssignedTo = nil
		task.AcceptedAt = nil
		task.StartedAt = nil
		task.ClaimedBy = nil
		task.ClaimedAt = nil
		res, err = d.conn.Exec(
			"UPDATE tasks SET status = ?, assigned_to = NULL, accepted_at = NULL, started_at = NULL, claimed_by = NULL, claimed_at = NULL, lease_holder = NULL, lease_expires_at = NULL, lease_heartbeat_at = NULL WHERE id = ? AND project = ? AND status = ?",
			newStatus, taskID, project, oldStatus,
		)
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
		res, err = d.conn.Exec(
			"UPDATE tasks SET status = ?, assigned_to = NULL, accepted_at = NULL, started_at = NULL, completed_at = NULL, result = NULL, blocked_reason = NULL, claimed_by = NULL, claimed_at = NULL, in_review_at = NULL, done_at = NULL, blocked_periods = '[]' WHERE id = ? AND project = ? AND status = ?",
			newStatus, taskID, project, oldStatus,
		)
	case "accepted":
		// claim → claimed_at + claimed_by (also sets assigned_to + accepted_at)
		task.AssignedTo = &agentName
		task.AcceptedAt = &now
		task.ClaimedBy = &agentName
		task.ClaimedAt = &now
		res, err = d.conn.Exec(
			"UPDATE tasks SET status = ?, assigned_to = ?, accepted_at = ?, claimed_by = ?, claimed_at = ? WHERE id = ? AND project = ? AND status = ?",
			newStatus, agentName, now, agentName, now, taskID, project, oldStatus,
		)
	case "in-progress":
		// start → started_at (and close any open blocked window on resume)
		task.AssignedTo = &agentName
		task.StartedAt = &now
		if leavingBlocked {
			task.BlockedPeriods = closeBlockedPeriod(task.BlockedPeriods, now)
			res, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, assigned_to = ?, started_at = ?, blocked_periods = ? WHERE id = ? AND project = ? AND status = ?",
				newStatus, agentName, now, task.BlockedPeriods, taskID, project, oldStatus,
			)
		} else {
			res, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, assigned_to = ?, started_at = ? WHERE id = ? AND project = ? AND status = ?",
				newStatus, agentName, now, taskID, project, oldStatus,
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
			res, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, assigned_to = COALESCE(assigned_to, ?), in_review_at = ?, blocked_periods = ? WHERE id = ? AND project = ? AND status = ?",
				newStatus, agentName, now, task.BlockedPeriods, taskID, project, oldStatus,
			)
		} else {
			res, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, assigned_to = COALESCE(assigned_to, ?), in_review_at = ? WHERE id = ? AND project = ? AND status = ?",
				newStatus, agentName, now, taskID, project, oldStatus,
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
			res, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, result = ?, completed_at = ?, done_at = ?, blocked_periods = ? WHERE id = ? AND project = ? AND status = ?",
				newStatus, result, now, now, task.BlockedPeriods, taskID, project, oldStatus,
			)
		} else {
			res, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, result = ?, completed_at = ?, done_at = ? WHERE id = ? AND project = ? AND status = ?",
				newStatus, result, now, now, taskID, project, oldStatus,
			)
		}
	case "blocked":
		// block → append {start: now} to blocked_periods
		task.BlockedReason = blockedReason
		task.BlockedPeriods = openBlockedPeriod(task.BlockedPeriods, now)
		res, err = d.conn.Exec(
			"UPDATE tasks SET status = ?, blocked_reason = ?, blocked_periods = ? WHERE id = ? AND project = ? AND status = ?",
			newStatus, blockedReason, task.BlockedPeriods, taskID, project, oldStatus,
		)
	case "cancelled":
		task.CompletedAt = &now
		task.BlockedReason = blockedReason // reuse as cancellation reason
		if leavingBlocked {
			task.BlockedPeriods = closeBlockedPeriod(task.BlockedPeriods, now)
			res, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, blocked_reason = ?, completed_at = ?, blocked_periods = ? WHERE id = ? AND project = ? AND status = ?",
				newStatus, blockedReason, now, task.BlockedPeriods, taskID, project, oldStatus,
			)
		} else {
			res, err = d.conn.Exec(
				"UPDATE tasks SET status = ?, blocked_reason = ?, completed_at = ? WHERE id = ? AND project = ? AND status = ?",
				newStatus, blockedReason, now, taskID, project, oldStatus,
			)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("update task status: %w", err)
	}
	// CAS check: 0 rows affected means the row changed (or vanished) between the
	// read and the write — a concurrent transition won. Report the conflict
	// instead of returning a task the caller never actually transitioned. This is
	// what stops two agents double-claiming the same task.
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return nil, newTaskError(CodeTaskStateConflict,
			"status changed from %q before %q could apply on task %s", oldStatus, newStatus, taskID)
	}
	// A transition is activity — reset the stale clock.
	_, _ = d.conn.Exec("UPDATE tasks SET last_activity_at = ? WHERE id = ? AND project = ?", now, taskID, project)
	task.LastActivityAt = &now

	// Lease bookkeeping runs in the same single-writer critical section, right
	// after the status CAS landed, so no other writer interleaves between the two.
	d.applyLeaseOnTransition(task, newStatus, agentName, priorHolder, now)
	return task, nil
}

// applyLeaseOnTransition maintains the task lease as a side effect of a status
// transition, mutating the in-memory task and its row in lockstep:
//   - accepted/in-progress/in-review by the working agent → hold + push expiry
//     (implicit heartbeat: any forward transition by the holder extends it);
//   - done/blocked/cancelled/pending → RELEASE (clear holder + expiry). If a
//     holder existed, stamp task.LeaseTransfer{from,to:"",reason:voluntary} so
//     the handler can emit task.lease_transferred, and record it to the audit
//     trail (nothing leaves a holder without a trace).
//
// Writes are best-effort follow-ups: the status transition already committed, so
// a lease write failure must not fail the transition — it only degrades the
// lease's freshness, self-heals on the next transition, and the expiry backstop
// still bounds a stale holder.
func (d *DB) applyLeaseOnTransition(task *models.Task, newStatus, agentName, priorHolder, now string) {
	switch newStatus {
	case "accepted", "in-progress", "in-review":
		expires := time.Now().UTC().Add(DefaultLeaseTTL).Format(memoryTimeFmt)
		if _, err := d.conn.Exec(
			"UPDATE tasks SET lease_holder = ?, lease_expires_at = ?, lease_heartbeat_at = ? WHERE id = ? AND project = ?",
			agentName, expires, now, task.ID, task.Project,
		); err == nil {
			task.LeaseHolder = &agentName
			task.LeaseExpiresAt = &expires
			hb := now
			task.LeaseHeartbeatAt = &hb
		}
	case "done", "blocked", "cancelled", "pending":
		if _, err := d.conn.Exec(
			"UPDATE tasks SET lease_holder = NULL, lease_expires_at = NULL, lease_heartbeat_at = NULL WHERE id = ? AND project = ?",
			task.ID, task.Project,
		); err == nil {
			task.LeaseHolder = nil
			task.LeaseExpiresAt = nil
			task.LeaseHeartbeatAt = nil
		}
		if priorHolder != "" {
			transfer := &models.LeaseTransfer{From: priorHolder, To: "", Reason: "voluntary"}
			task.LeaseTransfer = transfer
			d.auditLeaseTransfer(task.Project, task.ID, agentName, transfer)
		}
	}
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

// GetUnackedTasks returns pending tasks older than minAge that haven't been
// notified yet. Excludes run containers (run_state set): a container parent
// deliberately stays status='pending' while its run_state advances through the
// run lifecycle — it is never claimed/started (guardNotRunContainer refuses
// that), so treating it as an unacked idle task and nagging/escalating it to
// "consider re-dispatching" would be a false alarm every tick. This is a
// point-in-time filter only; a container can still start (or finish) a run
// after this read, which is why the mark* calls below re-check before acting.
func (d *DB) GetUnackedTasks(minAge time.Duration) ([]models.Task, error) {
	cutoff := time.Now().UTC().Add(-minAge).Format(memoryTimeFmt)
	rows, err := d.ro().Query(
		"SELECT "+taskColumns+" FROM tasks WHERE status = 'pending' AND archived_at IS NULL "+
			"AND dispatched_at < ? AND (run_state IS NULL OR run_state = '')",
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

// MarkTaskAckNotified sets the ack_notified_at timestamp — CAS-guarded so the
// ACK-checker's read-then-act window can't fire a stale notification: the batch
// GetUnackedTasks reads can be seconds old by the time each task is acted on
// (a full sweep, one write per task), during which the task could have become a
// run container (set_run stamped run_state), left 'pending' (claimed), or
// already been marked by a concurrent tick. The guard re-checks all of that at
// write time; ok=false means the caller must no-op (skip the notify), not act
// on the stale read.
func (d *DB) MarkTaskAckNotified(taskID string) (ok bool, err error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	res, err := d.conn.Exec(
		`UPDATE tasks SET ack_notified_at = ? WHERE id = ? AND status = 'pending'
		 AND (run_state IS NULL OR run_state = '') AND ack_notified_at IS NULL`,
		now, taskID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkTaskAckEscalated sets the ack_escalated_at timestamp — same CAS guard as
// MarkTaskAckNotified, see its doc comment.
func (d *DB) MarkTaskAckEscalated(taskID string) (ok bool, err error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	res, err := d.conn.Exec(
		`UPDATE tasks SET ack_escalated_at = ? WHERE id = ? AND status = 'pending'
		 AND (run_state IS NULL OR run_state = '') AND ack_escalated_at IS NULL`,
		now, taskID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
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

// DeleteTask hard-deletes a task and its owned child rows. task_progress_notes is
// the only table whose rows are OWNED by (reachable only via) a task, so a bare
// task-row delete leaves them as logical orphans keyed to a vanished task id —
// never queryable, never cleanable. Delete them child-first in one tx (the
// DeleteProject-cascade precedent; ordering satisfies any FK at each step).
//
// NOT touched, on purpose: messages.task_id is a soft cross-reference — a message
// about a task is an independent entity with its own deliveries (the one enforced
// FK) + reads, and legitimately outlives the task; cascading into it would over-
// delete. tasks.parent_task_id (subtasks) is a soft link — deleting a parent must
// not silently nuke its children. Both are left inert. No schema migration
// (52f7502d precedent).
func (d *DB) DeleteTask(taskID, project string) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("delete task begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM task_progress_notes WHERE task_id = ? AND project = ?", taskID, project); err != nil {
		return fmt.Errorf("delete task progress notes: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM tasks WHERE id = ? AND project = ?", taskID, project); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete task commit: %w", err)
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
	// An orchestrator reassign is an authoritative (voluntary) hand-off: it moves
	// the lease to the new agent too, so the lease never dangles on the prior
	// holder after the task has been handed on.
	priorHolder := strVal(task.LeaseHolder)
	now := time.Now().UTC().Format(memoryTimeFmt)
	expires := time.Now().UTC().Add(DefaultLeaseTTL).Format(memoryTimeFmt)
	// Reassignment recomputes profile_slug from the NEW assignee's registered
	// profile so display + skill-routing follow the hand-off (T3). Guard: only
	// when the new assignee actually has a non-empty registered slug — an empty
	// lookup must never blank the task's existing profile_slug.
	newSlug, hasSlug := d.profileSlugForAgent(project, agent)
	setCols := "assigned_to = ?, claimed_by = ?, lease_holder = ?, lease_expires_at = ?, lease_heartbeat_at = ?, last_activity_at = ?"
	args := []any{agent, agent, agent, expires, now, now}
	if hasSlug {
		setCols += ", profile_slug = ?"
		args = append(args, newSlug)
	}
	args = append(args, taskID, project)
	if _, err = d.conn.Exec(
		"UPDATE tasks SET "+setCols+" WHERE id = ? AND project = ?", args...,
	); err != nil {
		return nil, fmt.Errorf("reassign task: %w", err)
	}
	if hasSlug {
		task.ProfileSlug = newSlug
	}
	task.AssignedTo = &agent
	task.ClaimedBy = &agent
	task.LeaseHolder = &agent
	task.LeaseExpiresAt = &expires
	task.LeaseHeartbeatAt = &now
	if priorHolder != agent {
		transfer := &models.LeaseTransfer{From: priorHolder, To: agent, Reason: "voluntary"}
		task.LeaseTransfer = transfer
		d.auditLeaseTransfer(project, taskID, agent, transfer)
	}
	return task, nil
}

// profileSlugForAgent returns an agent's registered profile_slug and whether it
// is usable (a registered agent with a NON-EMPTY slug). Reassignment recomputes
// a task's profile_slug from its new assignee so display + skill routing follow
// the hand-off (T3); the bool lets callers skip the update when the new assignee
// has no registered profile, so an empty result never blanks an existing slug.
// Names are stored lowercase (migrateLowercaseAgentNames), so the lookup lowers.
func (d *DB) profileSlugForAgent(project, agent string) (string, bool) {
	var slug sql.NullString
	err := d.ro().QueryRow(
		"SELECT profile_slug FROM agents WHERE name = ? AND project = ?",
		strings.ToLower(strings.TrimSpace(agent)), project,
	).Scan(&slug)
	if err != nil || !slug.Valid || slug.String == "" {
		return "", false
	}
	return slug.String, true
}

// BackfillTaskProfileSlugs is a one-time, idempotent repair (T3): for every
// active task whose assignee has a registered, non-empty profile_slug that
// differs from the task's stored profile_slug, it recomputes profile_slug from
// the current assignee. Additive + guarded — no schema change, no destructive
// write — and re-running is a no-op once rows are consistent. Fixes rows made
// stale before ReassignTask learned to recompute. Returns the rows changed.
// The join is case-insensitive so a differently-cased assigned_to still matches.
func (d *DB) BackfillTaskProfileSlugs() (int, error) {
	return backfillTaskProfileSlugs(d.conn)
}

// backfillTaskProfileSlugs is the single SQL source shared by the exported
// method (tests / manual re-run) and the one-shot migrate() call — so the
// recompute predicate can never drift between the two.
func backfillTaskProfileSlugs(conn *sql.DB) (int, error) {
	res, err := conn.Exec(`
		UPDATE tasks
		SET profile_slug = (
			SELECT a.profile_slug FROM agents a
			WHERE LOWER(a.name) = LOWER(tasks.assigned_to) AND a.project = tasks.project
		)
		WHERE assigned_to IS NOT NULL AND assigned_to <> ''
		  AND archived_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM agents a
			WHERE LOWER(a.name) = LOWER(tasks.assigned_to) AND a.project = tasks.project
			  AND a.profile_slug IS NOT NULL AND a.profile_slug <> ''
			  AND a.profile_slug <> tasks.profile_slug
		  )`)
	if err != nil {
		return 0, fmt.Errorf("backfill task profile_slug: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
