package db

import (
	"database/sql"
	"log"
)

// ShortParentRef is a task whose parent_task_id is not a full uuid — the
// dangling links dispatch_task stored verbatim before task 98be27bb, when a
// caller passed a short id prefix. Candidates are the tasks in the same project
// whose id starts with the stored prefix.
type ShortParentRef struct {
	TaskID     string
	Project    string
	Ref        string
	Candidates []string
}

// listShortParentRefs reports every task whose parent_task_id is set but is not
// 36 characters long, with its same-project prefix matches.
func listShortParentRefs(conn *sql.DB) ([]ShortParentRef, error) {
	rows, err := conn.Query(`SELECT id, project, parent_task_id FROM tasks
		WHERE parent_task_id IS NOT NULL AND parent_task_id <> '' AND length(parent_task_id) <> 36
		ORDER BY dispatched_at, id`)
	if err != nil {
		return nil, err
	}
	var refs []ShortParentRef
	for rows.Next() {
		var r ShortParentRef
		if err := rows.Scan(&r.TaskID, &r.Project, &r.Ref); err != nil {
			_ = rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	for i := range refs {
		c, err := conn.Query(`SELECT id FROM tasks WHERE project = ? AND id LIKE ? AND length(id) = 36 ORDER BY id`,
			refs[i].Project, refs[i].Ref+"%")
		if err != nil {
			return nil, err
		}
		for c.Next() {
			var id string
			if err := c.Scan(&id); err != nil {
				_ = c.Close()
				return nil, err
			}
			refs[i].Candidates = append(refs[i].Candidates, id)
		}
		if err := c.Err(); err != nil {
			_ = c.Close()
			return nil, err
		}
		_ = c.Close()
	}
	return refs, nil
}

// backfillShortParents rewrites each short parent_task_id that matches exactly
// one task in the same project to that task's full id, and logs every ref it
// leaves alone (no match, or an ambiguous prefix). Idempotent: a repaired row
// is 36 characters and no longer selected. Returns (repaired, left).
func backfillShortParents(conn *sql.DB) (int, int, error) {
	refs, err := listShortParentRefs(conn)
	if err != nil {
		return 0, 0, err
	}
	repaired, left := 0, 0
	for _, r := range refs {
		if len(r.Candidates) != 1 {
			left++
			log.Printf("migrate: short parent_task_id %q on task %s (project %s) left as-is: %d matching task(s)",
				r.Ref, r.TaskID, r.Project, len(r.Candidates))
			continue
		}
		if _, err := conn.Exec(`UPDATE tasks SET parent_task_id = ? WHERE id = ? AND parent_task_id = ?`,
			r.Candidates[0], r.TaskID, r.Ref); err != nil {
			return repaired, left, err
		}
		repaired++
		log.Printf("migrate: short parent_task_id %q on task %s (project %s) -> %s", r.Ref, r.TaskID, r.Project, r.Candidates[0])
	}
	return repaired, left, nil
}

// runShortParentBackfill runs backfillShortParents once (settings marker, same
// pattern as runProductBoardRoutingBackfill) and logs the totals.
func runShortParentBackfill(conn *sql.DB) {
	var done string
	_ = conn.QueryRow("SELECT value FROM settings WHERE key = 'backfill_short_parent_ids'").Scan(&done)
	if done != "" {
		return
	}
	repaired, left, err := backfillShortParents(conn)
	if err != nil {
		log.Printf("migrate: short parent_task_id backfill failed: %v", err)
		return // marker unset: retried next boot
	}
	log.Printf("migrate: short parent_task_id backfill — %d repaired, %d left as-is", repaired, left)
	_, _ = conn.Exec("INSERT INTO settings (key, value) VALUES ('backfill_short_parent_ids', 'done') ON CONFLICT(key) DO UPDATE SET value = 'done'")
}
