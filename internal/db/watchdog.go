package db

import (
	"time"

	"agent-relay/internal/models"
)

// DefaultStuckThreshold is the conservative default idle window before an agent
// holding in-flight work is a stuck-candidate. It matches the health "dead"
// cutoff (health.go): a heads-down agent on a long build still bumps last_seen
// on its relay calls, so 30 min of total silence WHILE holding a task is the
// signal that its process is gone, not merely busy. Deliberately generous — the
// watchdog reclaims a genuinely dead lane, it is not a tight liveness probe.
const DefaultStuckThreshold = 30 * time.Minute

// StuckTask is one in-flight task a stuck agent is still holding.
type StuckTask struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// StuckAgent is a liveness-watchdog candidate: an agent that has not emitted any
// relay activity within the threshold yet still holds in-flight work. It is the
// READ the host daemon polls (inbound-only, relay-inbound-only-no-outbound-gh):
// the relay only EXPOSES the candidate set; the daemon owns the graceful
// pane-kill, then calls RequeueTask to return the work to the pool.
type StuckAgent struct {
	Agent       string      `json:"agent"`
	Project     string      `json:"project"`
	LastSeen    string      `json:"last_seen"`
	IdleSeconds int64       `json:"idle_seconds"`
	Tasks       []StuckTask `json:"tasks"`
}

// StuckAgents returns the stuck-candidate set for a project: every agent whose
// last_seen is older than threshold AND that still holds at least one in-flight
// task (accepted/in-progress/in-review). Deactivated agents (gracefully brought
// down) and sleeping agents (intentionally parked) are excluded — only a silent
// holder of live work is a candidate. Read-only (RO pool); never mutates.
func (d *DB) StuckAgents(project string, threshold time.Duration) ([]StuckAgent, error) {
	if threshold <= 0 {
		threshold = DefaultStuckThreshold
	}
	now := time.Now().UTC()
	// last_seen strictly before this instant is over-threshold. Compare in Go
	// (last_seen has two writer formats — see parseAgentTime) rather than in SQL.
	rows, err := d.ro().Query(`
		SELECT a.name, a.last_seen, t.id, t.title, t.status
		FROM agents a
		JOIN tasks t ON t.assigned_to = a.name AND t.project = a.project
		WHERE a.project = ?
		  AND a.status IN ('active','inactive')
		  AND t.status IN ('accepted','in-progress','in-review')
		  AND t.archived_at IS NULL
		ORDER BY a.name, t.id`, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// Preserve first-seen order while grouping tasks under each agent.
	byAgent := map[string]*StuckAgent{}
	var order []string
	for rows.Next() {
		var name, lastSeen, taskID, title, status string
		if err := rows.Scan(&name, &lastSeen, &taskID, &title, &status); err != nil {
			continue
		}
		idle := healthDeadAfter + time.Minute // unparseable last_seen → treat as dead
		if t, ok := parseAgentTime(lastSeen); ok {
			if idle = now.Sub(t); idle < 0 {
				idle = 0
			}
		}
		if idle < threshold {
			continue // seen recently → not stuck, even while holding work
		}
		sa, ok := byAgent[name]
		if !ok {
			sa = &StuckAgent{
				Agent:       name,
				Project:     project,
				LastSeen:    lastSeen,
				IdleSeconds: int64(idle.Seconds()),
			}
			byAgent[name] = sa
			order = append(order, name)
		}
		sa.Tasks = append(sa.Tasks, StuckTask{ID: taskID, Title: title, Status: status})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]StuckAgent, 0, len(order))
	for _, name := range order {
		out = append(out, *byAgent[name])
	}
	return out, nil
}

// RequeueTask returns a stuck holder's in-flight task to the pending pool so a
// fresh agent can claim it. It is the write-back half of the liveness watchdog:
// the daemon kills the dead pane, then requeues its work here.
//
// Unlike ReclaimTask (which hands the task to a NEW live holder) this releases
// ownership entirely — status → pending, assignee + lease cleared — so the task
// re-enters normal dispatch. CAS-guarded on (id, project, status) so a task that
// changed since it was read (the agent revived and completed it, a concurrent
// requeue) loses the race with TASK_STATE_CONFLICT rather than clobbering a
// legitimate transition. Only a held task (accepted/in-progress/in-review) is
// requeuable; a terminal or already-pending task is refused.
func (d *DB) RequeueTask(taskID, project, reason string) (*models.Task, error) {
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
	switch task.Status {
	case "accepted", "in-progress", "in-review":
		// held → requeuable
	default:
		return nil, newTaskError(CodeTaskStateConflict,
			"task %s is not in a requeuable state (status %q); requeue releases a held task back to pending", taskID, task.Status)
	}

	priorHolder := strVal(task.AssignedTo)
	oldStatus := task.Status
	res, err := d.conn.Exec(
		`UPDATE tasks SET status = 'pending', assigned_to = NULL, accepted_at = NULL,
		   started_at = NULL, in_review_at = NULL, claimed_by = NULL, claimed_at = NULL,
		   lease_holder = NULL, lease_expires_at = NULL, lease_heartbeat_at = NULL,
		   last_activity_at = ?
		 WHERE id = ? AND project = ? AND status = ?`,
		now, taskID, project, oldStatus,
	)
	if err != nil {
		return nil, err
	}
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return nil, newTaskError(CodeTaskStateConflict,
			"task %s changed before requeue could apply", taskID)
	}

	if reason == "" {
		reason = "stuck-watchdog"
	}
	transfer := &models.LeaseTransfer{From: priorHolder, To: "", Reason: reason}
	d.auditLeaseTransfer(project, taskID, "watchdog", transfer)

	fresh, err := d.GetTask(taskID, project)
	if err != nil || fresh == nil {
		return nil, err
	}
	fresh.LeaseTransfer = transfer
	return fresh, nil
}
