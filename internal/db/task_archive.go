package db

import (
	"agent-relay/internal/models"
	"fmt"
	"time"
)

// ArchiveTask soft-deletes ONE task by id — the reversible, audited per-task
// counterpart to the bulk ArchiveTasks. It stamps tasks.archived_at only when
// the row is currently active (a single-writer CAS on archived_at IS NULL) and,
// on that transition, writes exactly one "task.archived" audit row. A missing
// or already-archived task is a no-op: (false, nil) with NO audit written, so
// the caller distinguishes 404 (task gone) from 409 (already archived) by its
// own prior read.
//
// A non-empty reason is required: an archive with no "why" is refused before
// any write, so the audit trail never carries a blank reason. Any status is
// archivable — this is a soft delete, not a lifecycle transition.
func (d *DB) ArchiveTask(project, id, reason, actor string) (bool, error) {
	if reason == "" {
		return false, fmt.Errorf("archive task: reason required")
	}
	now := time.Now().UTC().Format(memoryTimeFmt)
	result, err := d.writerExec(
		"UPDATE tasks SET archived_at = ? WHERE id = ? AND project = ? AND archived_at IS NULL",
		now, id, project,
	)
	if err != nil {
		return false, fmt.Errorf("archive task: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("archive task: %w", err)
	}
	if n == 0 {
		// Not found or already archived — nothing changed, so no audit row.
		return false, nil
	}
	// RecordAudit auto-derives trace_id and defaults ResourceType; best-effort
	// by contract, so a logging failure must not fail the archive.
	_ = d.RecordAudit(models.AuditEntry{
		Action:       "task.archived",
		Actor:        actor,
		Project:      project,
		ResourceType: "task",
		ResourceID:   id,
		Reason:       reason,
	})
	return true, nil
}
