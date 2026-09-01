package db

import (
	"fmt"
	"time"
)

// AgentRef identifies an agent row by its natural key (project + name).
type AgentRef struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

// ReviewerPurgeResult reports what a PurgeStaleReviewers pass matched. Candidates
// is the set that was (or, on a dry run, would be) soft-deleted; Purged is the
// rows actually mutated (0 on a dry run).
type ReviewerPurgeResult struct {
	Candidates []AgentRef `json:"candidates"`
	Purged     int        `json:"purged"`
	DryRun     bool       `json:"dry_run"`
}

// PurgeStaleReviewers soft-deletes dead ephemeral reviewer agents (name LIKE
// 'review-%') that the niwa gate registers per review and never tears down
// (DEC-niwa-reviewer-lifecycle-1) — ~385 inactive rows had accumulated across 10
// projects on synx-prod. It must run IN the relay process (writer connection):
// an external raw-SQL UPDATE would be a second writer on the single-writer DB and
// would not be reflected in the running relay's own view.
//
// Safety: only ever touches status='inactive' review-* rows (an 'active' one is
// never matched), HARD-SKIPS any agent still holding a live task
// (accepted/in-progress/in-review), and SOFT-deletes (status='deleted', row
// kept, deactivated_at preserved) so it is fully recoverable — never a hard row
// delete. Idempotent: a re-run finds nothing (the rows are no longer 'inactive').
// olderThan>0 additionally requires the agent to have gone inactive (or last been
// seen) before now-olderThan — used by the standing TTL reaper; olderThan==0
// matches every inactive reviewer, used by the one-shot backlog purge.
func (d *DB) PurgeStaleReviewers(dryRun bool, olderThan time.Duration) (ReviewerPurgeResult, error) {
	res := ReviewerPurgeResult{DryRun: dryRun}

	// Shared predicate: an inactive review-* not holding any live task. Used for
	// both the candidate SELECT and the mutating UPDATE so the two never diverge
	// (skip-live is re-checked at mutation time, closing the TOCTOU window).
	where := `name LIKE 'review-%' AND status = 'inactive'
		AND NOT EXISTS (
			SELECT 1 FROM tasks t
			WHERE t.claimed_by = agents.name
			  AND t.project = agents.project
			  AND t.status IN ('accepted','in-progress','in-review')
		)`
	var whereArgs []any
	if olderThan > 0 {
		cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
		where += ` AND COALESCE(deactivated_at, last_seen) <> ''
			AND datetime(COALESCE(deactivated_at, last_seen)) < datetime(?)`
		whereArgs = append(whereArgs, cutoff)
	}

	rows, err := d.ro().Query(`SELECT project, name FROM agents WHERE `+where, whereArgs...)
	if err != nil {
		return res, fmt.Errorf("select stale reviewers: %w", err)
	}
	for rows.Next() {
		var a AgentRef
		if err := rows.Scan(&a.Project, &a.Name); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("scan stale reviewer: %w", err)
		}
		res.Candidates = append(res.Candidates, a)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("iterate stale reviewers: %w", err)
	}

	if dryRun || len(res.Candidates) == 0 {
		return res, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.writerExec(
		`UPDATE agents SET status = 'deleted', deactivated_at = COALESCE(deactivated_at, ?) WHERE `+where,
		append([]any{now}, whereArgs...)...,
	)
	if err != nil {
		return res, fmt.Errorf("purge stale reviewers: %w", err)
	}
	n, _ := result.RowsAffected()
	res.Purged = int(n)
	return res, nil
}
