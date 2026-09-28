package db

import (
	"fmt"
	"time"
)

// Stale-held alert (task 887351ac): the ACK ladder only watches pending
// tasks, so work that was claimed and then went silent (a dead or wedged
// holder) never surfaced. A task accepted / in-progress with no heartbeat or
// activity since the cutoff alerts its dispatcher once per stale episode.
// in-review is excluded: it is waiting on the gate, not on its holder.

// staleSeen is the last sign of life of task t: activity, lease heartbeat,
// start or claim (memoryTimeFmt, so MAX compares).
const staleSeen = `MAX(COALESCE(last_activity_at, ''), COALESCE(lease_heartbeat_at, ''), COALESCE(started_at, ''), COALESCE(accepted_at, ''), COALESCE(dispatched_at, ''))`

// staleUnalerted: no alert yet for this episode (none, or one older than the
// last sign of life).
const staleUnalerted = `(stale_notified_at IS NULL OR stale_notified_at < ` + staleSeen + `)`

// StaleHeldTask is one held task gone silent.
type StaleHeldTask struct {
	ID, Project, Title, DispatchedBy, Holder, Status, Seen string
}

// StaleHeldTasks lists accepted / in-progress tasks silent since cutoff and
// not yet alerted for this episode. Read-only.
func (d *DB) StaleHeldTasks(cutoff time.Time) ([]StaleHeldTask, error) {
	rows, err := d.ro().Query(`SELECT id, project, COALESCE(title, ''), dispatched_by,
			COALESCE(lease_holder, assigned_to, claimed_by, ''), status, `+staleSeen+`
		FROM tasks
		WHERE status IN ('accepted', 'in-progress') AND archived_at IS NULL
		  AND `+staleSeen+` < ? AND `+staleUnalerted+`
		ORDER BY `+staleSeen+`, id`, cutoff.UTC().Format(memoryTimeFmt))
	if err != nil {
		return nil, fmt.Errorf("stale held tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []StaleHeldTask
	for rows.Next() {
		var s StaleHeldTask
		if err := rows.Scan(&s.ID, &s.Project, &s.Title, &s.DispatchedBy, &s.Holder, &s.Status, &s.Seen); err != nil {
			return nil, fmt.Errorf("scan stale held task: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkStaleNotified claims the alert for one stale episode. CAS on the
// status, the last sign of life read (seen) and the unalerted guard, so a
// task that moved or woke up since the read is not alerted, and two sweeps
// never both alert. false = someone else won or the task changed.
func (d *DB) MarkStaleNotified(id, project, seen string, now time.Time) (bool, error) {
	res, err := d.writerExec(`UPDATE tasks SET stale_notified_at = ?
		WHERE id = ? AND project = ? AND status IN ('accepted', 'in-progress')
		  AND `+staleSeen+` = ? AND `+staleUnalerted,
		now.UTC().Format(memoryTimeFmt), id, project, seen)
	if err != nil {
		return false, fmt.Errorf("mark stale notified: %w", err)
	}
	n, err := res.RowsAffected()
	return err == nil && n == 1, err
}
