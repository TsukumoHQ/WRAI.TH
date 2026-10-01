package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-relay/internal/models"
)

// W8 D1 (ruling wraith-deploying-ruling, docs/design/deploying-status.md):
// 'deploying' = merged in a pipeline repo, the post-merge pipeline is deploying
// and verifying it. The doer keeps the lease (N3: lifecycle moves never move
// it), the sweeper never reclaims it (it sweeps accepted/in-progress/in-review
// only), WIP and ACK ignore it. task_deploys records the merge sha that entered
// deploying, so a retried deploy out of blocked is allowed only for that sha.

func migrateDeploying(conn *sql.DB) {
	_, _ = conn.Exec(`CREATE TABLE IF NOT EXISTS task_deploys (
		task_id    TEXT PRIMARY KEY,
		project    TEXT NOT NULL,
		merge_sha  TEXT NOT NULL,
		entered_at TEXT NOT NULL
	)`)
}

// lastBlockedFrom is the status the task's open (last) blocked window was
// entered from, "" when it has none.
func lastBlockedFrom(periods string) string {
	var ps []blockedPeriod
	if periods == "" || json.Unmarshal([]byte(periods), &ps) != nil || len(ps) == 0 {
		return ""
	}
	return ps[len(ps)-1].From
}

// DeployTask moves a task to deploying: from in-review (the merge landed;
// mergeSHA is recorded) or from blocked (a retried deploy: only when the task
// was blocked out of deploying and mergeSHA is the sha that entered it).
// Fenced like review: the lease holder or an override actor (niwa).
func (d *DB) DeployTask(taskID, agentName, project, mergeSHA string) (*models.Task, error) {
	return d.DeployTaskFenced(taskID, agentName, project, mergeSHA, nil)
}

// DeployTaskFenced is DeployTask with the caller's lease generation (nil = not
// supplied), fenced like ReviewTaskFenced.
func (d *DB) DeployTaskFenced(taskID, agentName, project, mergeSHA string, gen *int64) (*models.Task, error) {
	mergeSHA = strings.ToLower(strings.TrimSpace(mergeSHA))
	if mergeSHA == "" {
		return nil, fmt.Errorf("merge_sha is required to move task %s to deploying", taskID)
	}
	task, err := d.GetTask(taskID, project)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	from := task.Status
	if from == "blocked" {
		if lastBlockedFrom(task.BlockedPeriods) != "deploying" {
			return nil, newTaskError(CodeTaskStateConflict,
				"task %s was not blocked out of deploying; a first deploy goes through in-review", taskID)
		}
		var entered string
		err := d.ro().QueryRow(`SELECT merge_sha FROM task_deploys WHERE task_id = ? AND project = ?`, taskID, project).Scan(&entered)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("deploy task: read merge sha: %w", err)
		}
		if entered == "" || entered != mergeSHA {
			return nil, newTaskError(CodeTaskStateConflict,
				"task %s entered deploying at merge sha %q; a retried deploy must carry that sha, got %q", taskID, entered, mergeSHA)
		}
	}
	updated, err := d.transitionTaskCode(taskID, agentName, project, "deploying", nil, nil, "", gen)
	if err != nil {
		return nil, err
	}
	if from != "blocked" {
		// Best-effort follow-up: the status CAS already committed. A lost write
		// only makes a later blocked -> deploying retry refuse (fail closed).
		now := time.Now().UTC().Format(memoryTimeFmt)
		_, _ = d.writerExec(`INSERT INTO task_deploys (task_id, project, merge_sha, entered_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(task_id) DO UPDATE SET project = excluded.project, merge_sha = excluded.merge_sha, entered_at = excluded.entered_at`,
			taskID, project, mergeSHA, now)
	}
	return updated, nil
}

// DeployMergeSHA is the merge sha that last moved the task into deploying, ""
// when it never deployed.
func (d *DB) DeployMergeSHA(project, taskID string) string {
	var sha string
	_ = d.ro().QueryRow(`SELECT merge_sha FROM task_deploys WHERE task_id = ? AND project = ?`, taskID, project).Scan(&sha)
	return sha
}
