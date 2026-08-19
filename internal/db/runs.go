package db

import (
	"agent-relay/internal/models"
	"database/sql"
)

// Run-zone typed error codes (changeset-per-factory-run S1). Handlers surface
// Code so a client distinguishes a bad transition / a container-claim from a
// generic failure. Both map to CategoryValidation, non-retryable.
const (
	// CodeRunStateInvalid — a set_run tried a run_state edge not in the lifecycle
	// map (e.g. merged→open, or a first stamp to anything but open).
	CodeRunStateInvalid = "RUN_STATE_INVALID"
	// CodeRunContainer — an agent tried to claim/start a task that is a run
	// container (run_state set). The container groups slices; it is never leased
	// as work. Its child slices are the claimable units.
	CodeRunContainer = "RUN_CONTAINER_NOT_CLAIMABLE"
)

// Run lifecycle states (changeset-per-factory-run S1). The run is the PARENT
// task; these track its merge lifecycle, distinct from the task's own status.
const (
	RunStateOpen      = "open"      // run created, integration branch cut, slices in flight
	RunStateGating    = "gating"    // all slices landed on integration; tier-2 union-diff review
	RunStateMerging   = "merging"   // tier-2 green; daemon merging integration→trunk
	RunStateMerged    = "merged"    // terminal: integration merged to trunk
	RunStateBlocked   = "blocked"   // a slice red / run held; recoverable
	RunStateAmputated = "amputated" // a slice dropped (logged); green subset proceeds
)

// validRunTransitions is the run_state edge set, enforced by SetTaskRun. The
// empty state is "not yet a run" — the first stamp must open it (open-first).
// merged is terminal (no resurrection). blocked recovers or amputates; amputated
// lets the green subset proceed back through gating→merging→merged. Deliberately
// permissive within the forward DAG but never backward from a terminal — mirrors
// the PR-link no-resurrect posture. Coordinated with cto-tsukumo.
var validRunTransitions = map[string][]string{
	"":                {RunStateOpen},
	RunStateOpen:      {RunStateGating, RunStateBlocked},
	RunStateGating:    {RunStateMerging, RunStateBlocked},
	RunStateMerging:   {RunStateMerged, RunStateBlocked},
	RunStateBlocked:   {RunStateOpen, RunStateGating, RunStateAmputated, RunStateMerged},
	RunStateAmputated: {RunStateGating, RunStateMerging, RunStateMerged, RunStateBlocked},
	RunStateMerged:    {},
}

// runTransitionAllowed reports whether from→to is a legal run_state edge. A
// same-state stamp (from == to) is treated as an idempotent no-op by the caller
// and never reaches here.
func runTransitionAllowed(from, to string) bool {
	for _, s := range validRunTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// guardNotRunContainer refuses a worker claim/start on a run-container task (a
// task with run_state set). The container groups its child slices; it is never
// itself leased as work — folding cto-tsukumo's greenlight AC. A missing row is
// left to the transition to surface as the normal not-found, so this guard only
// rejects the container case.
func (d *DB) guardNotRunContainer(taskID, project string) error {
	var runState sql.NullString
	err := d.ro().QueryRow(
		"SELECT run_state FROM tasks WHERE id = ? AND project = ?",
		taskID, project,
	).Scan(&runState)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if runState.Valid && runState.String != "" {
		return newTaskError(CodeRunContainer,
			"task %s is a run container (run_state=%s): it groups the run's slices and is not claimable as work — claim a child slice instead",
			taskID, runState.String)
	}
	return nil
}

// SetTaskRun stamps the run zone on the PARENT task (changeset-per-factory-run
// S1). integrationBranch is COALESCE'd (a state-only advance never wipes the
// branch, and vice-versa), mirroring SetTaskPR. runState is transition-enforced
// against validRunTransitions: the first stamp must open the run, and no edge
// resurrects a merged run. A same-state stamp is an idempotent no-op. Nil args
// leave their column untouched. Returns the re-read task, or a typed
// CodeTaskNotFound / CodeRunStateInvalid error. Single UPDATE on the writer conn.
func (d *DB) SetTaskRun(taskID, project string, integrationBranch, runState *string) (*models.Task, error) {
	cur, err := d.GetTask(taskID, project)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, newTaskError(CodeTaskNotFound, "task not found: %s", taskID)
	}

	if runState != nil {
		from := ""
		if cur.RunState != nil {
			from = *cur.RunState
		}
		to := *runState
		if from != to && !runTransitionAllowed(from, to) {
			cause := "invalid run_state transition"
			if from == "" {
				cause = "run_state must open first"
			}
			return nil, newTaskError(CodeRunStateInvalid,
				"%s: %q → %q", cause, from, to)
		}
	}

	if _, err := d.conn.Exec(
		`UPDATE tasks SET
			integration_branch = COALESCE(?, integration_branch),
			run_state = COALESCE(?, run_state)
		 WHERE id = ? AND project = ?`,
		integrationBranch, runState, taskID, project,
	); err != nil {
		return nil, err
	}
	return d.GetTask(taskID, project)
}

// GetRun returns the run as the reviewer/agentd sees it: the PARENT task
// (carrying the run zone: integration_branch + run_state) with its subtask chain
// — the agent slices — attached. Thin, explicit alias over GetTaskWithSubtasks so
// S2/niwa has a named run read; the relay stays inbound-only (this is a pure DB
// read, no outbound GitHub). Returns nil when the id does not resolve. A run is
// simply a parent task whose run_state is set; this read does not require it, so
// a caller may inspect run_state to decide.
func (d *DB) GetRun(runID, project string) (*models.Task, error) {
	return d.GetTaskWithSubtasks(runID, project)
}
