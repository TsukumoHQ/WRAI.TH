package db

import (
	"agent-relay/internal/models"
	"fmt"
	"strings"
)

// WIP limit (task e2273dc3): an agent may hold at most wip_limit (per
// project, default 1, 0 = unlimited) tasks in accepted / in-progress — the
// ones it is actively working. in-review is not counted: the doer pulls its
// next ticket while the gate reviews the last one; a bounce back to
// in-progress counts again. Enforced on claim_task (and claim next, which
// claims through ClaimTask) and start_task, for the caller who ends up
// holding the lease.

// CodeWIPLimit — the caller already holds wip_limit active tasks.
const CodeWIPLimit = "WIP_LIMIT"

// ProjectWIPLimit is the project's wip_limit; 1 when the project row or the
// column is missing.
func (d *DB) ProjectWIPLimit(project string) int {
	var n int
	if err := d.ro().QueryRow(`SELECT wip_limit FROM projects WHERE name = ?`, canonicalProject(project)).Scan(&n); err != nil {
		return 1
	}
	return n
}

// SetProjectWIPLimit sets the project's wip_limit (0 = unlimited).
func (d *DB) SetProjectWIPLimit(project string, limit int) error {
	if limit < 0 {
		return fmt.Errorf("wip_limit must be >= 0 (0 = unlimited), got %d", limit)
	}
	res, err := d.writerExec(`UPDATE projects SET wip_limit = ? WHERE name = ?`, limit, canonicalProject(project))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("project %q not found", project)
	}
	return nil
}

// WIPTask is one task counted against an agent's WIP limit.
type WIPTask struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// activeHeld lists the accepted / in-progress tasks agent holds in project,
// except excludeID, oldest claim first.
func (d *DB) activeHeld(project, agent, excludeID string) ([]WIPTask, error) {
	rows, err := d.ro().Query(`SELECT id, COALESCE(title, ''), status FROM tasks
		WHERE project = ? AND lease_holder = ? AND id <> ? AND status IN ('accepted', 'in-progress') AND archived_at IS NULL
		ORDER BY COALESCE(claimed_at, accepted_at, dispatched_at), id`, project, agent, excludeID)
	if err != nil {
		return nil, fmt.Errorf("wip count: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WIPTask
	for rows.Next() {
		var w WIPTask
		if err := rows.Scan(&w.ID, &w.Title, &w.Status); err != nil {
			return nil, fmt.Errorf("wip scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// checkWIP refuses agent taking taskID when it already holds wip_limit other
// active tasks. An override actor acting on a task it does not hold (the gate
// daemon resuming a bounced ticket, the operator) never takes the lease, so
// it is not counted; force lets an override actor (the task's dispatcher,
// human, RELAY_OVERRIDE_ACTORS) exceed its own limit, audited wip_override.
//
// claiming: a claim always hands the lease to agent and is checked. A start is
// checked unless agent already holds that task's lease (accepted → start) or
// an override actor starts someone else's task (the lease stays with them) —
// so a pending start, even of a task queued to agent, and a resume of a task
// agent blocked are both counted.
func (d *DB) checkWIP(taskID, agent, project string, force, claiming bool) error {
	task, err := d.GetTask(taskID, project)
	if err != nil || task == nil {
		return err // the transition reports a missing task itself
	}
	override := isOverrideActor(task, agent)
	holder := strVal(task.LeaseHolder)
	worker := holder
	if worker == "" {
		worker = strVal(task.AssignedTo)
	}
	if !claiming && holder != "" && strings.EqualFold(holder, agent) {
		return nil // starting its own claimed task: decided at the claim (a forced claim stays forced)
	}
	if !claiming && override && worker != "" && !strings.EqualFold(worker, agent) {
		return nil // acting for the worker: the lease does not move to agent
	}
	limit := d.ProjectWIPLimit(project)
	if limit == 0 {
		return nil
	}
	held, err := d.activeHeld(project, agent, taskID)
	if err != nil || len(held) < limit {
		return err
	}
	if force && override {
		_ = d.RecordAudit(models.AuditEntry{
			Project:      project,
			Actor:        agent,
			Action:       "wip_override",
			ResourceType: "task",
			ResourceID:   taskID,
			Summary:      fmt.Sprintf("%s took %s over its wip_limit %d (holds %s)", agent, taskID, limit, wipIDs(held)),
			Reason:       "force",
		})
		return nil
	}
	return newTaskError(CodeWIPLimit,
		"%s already holds %d active task(s) (wip_limit %d in %s): %s — submit it (review_task) or block it before taking %s",
		agent, len(held), limit, project, wipIDs(held), taskID)
}

func wipIDs(held []WIPTask) string {
	ids := make([]string, len(held))
	for i, h := range held {
		ids[i] = h.ID
	}
	return strings.Join(ids, ", ")
}

// WIPOverAgent is one agent over its project's WIP limit, for the report.
type WIPOverAgent struct {
	Agent string    `json:"agent"`
	Limit int       `json:"limit"`
	Held  []WIPTask `json:"held"`
}

// WIPOverLimit lists the agents of project holding more active tasks than
// its wip_limit (report only: nothing is released). Empty when unlimited.
func (d *DB) WIPOverLimit(project string) ([]WIPOverAgent, error) {
	limit := d.ProjectWIPLimit(project)
	if limit == 0 {
		return nil, nil
	}
	rows, err := d.ro().Query(`SELECT lease_holder FROM tasks
		WHERE project = ? AND lease_holder IS NOT NULL AND lease_holder <> '' AND status IN ('accepted', 'in-progress') AND archived_at IS NULL
		GROUP BY lease_holder HAVING COUNT(*) > ? ORDER BY lease_holder`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("wip report: %w", err)
	}
	var agents []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("wip report scan: %w", err)
		}
		agents = append(agents, a)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]WIPOverAgent, 0, len(agents))
	for _, a := range agents {
		held, err := d.activeHeld(project, a, "")
		if err != nil {
			return nil, err
		}
		out = append(out, WIPOverAgent{Agent: a, Limit: limit, Held: held})
	}
	return out, nil
}
