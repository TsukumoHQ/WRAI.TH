package db

import (
	"agent-relay/internal/models"
	"fmt"
	"time"
)

// Typed task-error codes. Callers (handlers) surface Code to the client so a
// lost race is a distinguishable, non-retryable signal ("park", not "hot-loop")
// instead of a bare string. Everything mutating the task state machine returns a
// *TaskError on a guard failure.
const (
	// CodeTaskStateConflict — a compare-and-swap transition found the row already
	// changed (a concurrent transition won). Never a silent overwrite.
	CodeTaskStateConflict = "TASK_STATE_CONFLICT"
	// CodeTaskLeaseHeld — re-claim refused because the task's lease is still held
	// by a live holder (lease not expired AND holder registered/active).
	CodeTaskLeaseHeld = "TASK_LEASE_HELD"
	// CodeTaskNotFound — the task id does not resolve in the project.
	CodeTaskNotFound = "TASK_NOT_FOUND"
)

// TaskError is a typed error carrying a stable Code so handlers can map it to a
// structured client response (park vs retry) rather than pattern-matching a
// message string.
type TaskError struct {
	Code string
	Msg  string
}

func (e *TaskError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

func newTaskError(code, format string, a ...any) *TaskError {
	return &TaskError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// DefaultLeaseTTL is how long a claim/heartbeat keeps the task owned before the
// lease lapses. Deliberately generous (per CTO amendment: >= 2h) so a heads-down
// agent on a long build is not treated as dead and does not lose its task — the
// lease is a dead-holder backstop, not a tight liveness probe. Liveness is
// re-derived from the holder's registration/activity, not a separate heartbeat.
const DefaultLeaseTTL = 2 * time.Hour

// leaseHeld reports whether a task currently has a LIVE lease held by an agent
// OTHER than challenger — i.e. a re-claim must be refused. A lease is NOT live
// (re-claim allowed) when: there is no holder, the holder is the challenger
// itself, the lease has expired (expires_at < now), or the holder agent is
// deregistered/inactive (no live registration). now is the comparison instant
// (UTC, memoryTimeFmt).
func (d *DB) leaseHeld(project, holder, expiresAt, challenger, now string) (bool, string) {
	if holder == "" || holder == challenger {
		return false, ""
	}
	// Expired lease → re-claimable regardless of holder liveness.
	if expiresAt != "" && expiresAt < now {
		return false, "expired"
	}
	// Not expired: only re-claimable if the holder is confirmed dead.
	if !d.agentLive(project, holder) {
		return false, "deregistered"
	}
	return true, ""
}

// agentLive reports whether an agent is a live owner candidate: a registered row
// in an active/sleeping state. A missing row or a deleted/inactive status means
// dead (its task may be re-claimed). This is the "implicit heartbeat": the stale
// sweep (MarkStaleAgentsInactive) already flips a silent agent to inactive, so
// liveness rides existing activity signals rather than a dedicated heartbeat.
func (d *DB) agentLive(project, name string) bool {
	var status string
	err := d.ro().QueryRow(
		"SELECT status FROM agents WHERE name = ? AND project = ?", name, project,
	).Scan(&status)
	if err != nil {
		return false // no such agent → dead holder
	}
	return status == "active" || status == "sleeping"
}

// ReclaimTask transfers a DEAD holder's task to newAgent. It is the primitive
// niwa's supervisor-driven re-claim (resume-protocol part 3) stands on: it
// refuses (TASK_LEASE_HELD) when the current holder is still live, and otherwise
// atomically hands ownership over — status → accepted under the new holder with
// a fresh lease. The returned task carries LeaseTransfer{from,to,reason} so the
// handler can emit task.lease_transferred (SSE); the audit trail is written here.
//
// Atomicity: the decision is CAS-guarded on (id, project, lease_holder, status)
// so a second reclaim racing the first sees 0 rows and gets TASK_STATE_CONFLICT
// rather than double-transferring.
func (d *DB) ReclaimTask(taskID, newAgent, project string) (*models.Task, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)

	task, err := d.GetTask(taskID, project)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, newTaskError(CodeTaskNotFound, "task not found: %s", taskID)
	}
	// Linear-mirrored tasks are read-only here (Linear is SSOT).
	if task.Source == "linear" {
		return nil, errLinearReadOnly
	}

	// Reclaim only applies to a task actively HELD by a (possibly dead) worker —
	// the held-work states. A pending task has no holder: use claim_task. A
	// terminal (done/cancelled) or released (blocked) task must never be
	// resurrected to 'accepted' by a reclaim; refusing here is what stops that.
	switch task.Status {
	case "accepted", "in-progress", "in-review":
		// reclaimable
	default:
		return nil, newTaskError(CodeTaskStateConflict,
			"task %s is not in a reclaimable state (status %q); reclaim takes over a held task, use claim_task for a pending one", taskID, task.Status)
	}

	priorHolder := strVal(task.LeaseHolder)
	priorExpires := strVal(task.LeaseExpiresAt)
	held, reason := d.leaseHeld(project, priorHolder, priorExpires, newAgent, now)
	if held {
		return nil, newTaskError(CodeTaskLeaseHeld,
			"task %s lease is held by live agent %q (expires %s)", taskID, priorHolder, priorExpires)
	}
	if reason == "" {
		// No holder / same holder / not dead-by-expiry-or-deregistration but also
		// not live-held: treat a plain unheld reclaim as a voluntary (re)acquire.
		reason = "voluntary"
	}

	expires := time.Now().UTC().Add(DefaultLeaseTTL).Format(memoryTimeFmt)
	res, err := d.conn.Exec(
		`UPDATE tasks SET status = 'accepted', assigned_to = ?, claimed_by = ?, accepted_at = ?,
		   claimed_at = ?, lease_holder = ?, lease_expires_at = ?, lease_heartbeat_at = ?, last_activity_at = ?
		 WHERE id = ? AND project = ? AND COALESCE(lease_holder,'') = ? AND status = ?`,
		newAgent, newAgent, now, now, newAgent, expires, now, now,
		taskID, project, priorHolder, task.Status,
	)
	if err != nil {
		return nil, fmt.Errorf("reclaim task: %w", err)
	}
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return nil, newTaskError(CodeTaskStateConflict,
			"task %s changed before reclaim by %q could apply", taskID, newAgent)
	}

	transfer := &models.LeaseTransfer{From: priorHolder, To: newAgent, Reason: reason}
	d.auditLeaseTransfer(project, taskID, newAgent, transfer)

	// Re-read so the caller sees the fully-stamped row, then attach the transient
	// transfer marker (not persisted, not scanned).
	fresh, err := d.GetTask(taskID, project)
	if err != nil || fresh == nil {
		return nil, fmt.Errorf("reclaim task: reload: %w", err)
	}
	fresh.LeaseTransfer = transfer
	return fresh, nil
}

// auditLeaseTransfer records a lease change to the durable audit trail. Best
// effort: a failed audit write must never block the transfer it describes.
func (d *DB) auditLeaseTransfer(project, taskID, actor string, t *models.LeaseTransfer) {
	to := t.To
	if to == "" {
		to = "(released)"
	}
	_ = d.RecordAudit(models.AuditEntry{
		Project:      project,
		Actor:        actor,
		Action:       "lease_transferred",
		ResourceType: "task",
		ResourceID:   taskID,
		Summary:      fmt.Sprintf("lease %s → %s (%s)", orNone(t.From), to, t.Reason),
		Reason:       t.Reason,
	})
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// strVal dereferences a *string to its value or "".
func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
