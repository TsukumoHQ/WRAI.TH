package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Refusals of SetTaskParent. The handler maps each to its typed tool error.
var (
	ErrParentSelf     = errors.New("a task cannot be its own parent")
	ErrParentNotFound = errors.New("parent task not found in this project")
	ErrParentCycle    = errors.New("parent would create a cycle")
)

// maxParentWalk bounds the ancestor walk of the cycle check. Real chains are a
// few levels deep (get_task renders subtasks to depth 3); the bound only stops a
// corrupt pre-existing loop from spinning forever. Hitting it refuses the edit.
const maxParentWalk = 64

// SetTaskParent sets (parentID != "") or clears (parentID == "") a task's
// parent_task_id after dispatch (task eae0243b), so live tickets can be
// regrouped under an epic. parentID must already be resolved to a full id.
// The parent must exist in the same project, must not be the task itself, and
// must not have the task among its ancestors. Check and write run in one writer
// transaction, so two concurrent edits cannot jointly create a cycle. Nothing
// is inherited retroactively: only the link changes. The change is audited as
// a progress note "parent set to X by Y" / "parent cleared (was X) by Y".
// Returns the previous parent ("" when none).
func (d *DB) SetTaskParent(taskID, project, actor, parentID string) (string, error) {
	tx, err := d.beginWriterTx()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var old sql.NullString
	if err := tx.QueryRow("SELECT parent_task_id FROM tasks WHERE id = ? AND project = ?", taskID, project).Scan(&old); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("task not found: %s", taskID)
		}
		return "", err
	}

	if parentID == old.String {
		return old.String, nil // unchanged: no write, no audit noise
	}

	if parentID != "" {
		if parentID == taskID {
			return "", ErrParentSelf
		}
		// Walk up from the new parent: reaching the task itself means the task
		// is already an ancestor of its would-be parent.
		cur := parentID
		for i := 0; ; i++ {
			if i >= maxParentWalk {
				return "", fmt.Errorf("%w: ancestor chain of %s exceeds %d levels", ErrParentCycle, parentID, maxParentWalk)
			}
			var up sql.NullString
			err := tx.QueryRow("SELECT parent_task_id FROM tasks WHERE id = ? AND project = ?", cur, project).Scan(&up)
			if errors.Is(err, sql.ErrNoRows) {
				if cur == parentID {
					return "", ErrParentNotFound
				}
				break // dangling ancestor link: the chain ends here
			}
			if err != nil {
				return "", err
			}
			if !up.Valid || up.String == "" {
				break
			}
			if up.String == taskID {
				return "", fmt.Errorf("%w: %s is already an ancestor of %s", ErrParentCycle, taskID, parentID)
			}
			cur = up.String
		}
	}

	var newVal any
	note := fmt.Sprintf("parent cleared (was %s) by %s", old.String, actor)
	if parentID != "" {
		newVal = parentID
		note = fmt.Sprintf("parent set to %s by %s", parentID, actor)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec("UPDATE tasks SET parent_task_id = ?, last_activity_at = ? WHERE id = ? AND project = ?",
		newVal, now, taskID, project); err != nil {
		return "", fmt.Errorf("set parent: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_progress_notes (task_id, project, agent, note, created_at) VALUES (?, ?, ?, ?, ?)`,
		taskID, project, actor, note, now); err != nil {
		return "", fmt.Errorf("set parent: audit note: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("set parent: commit: %w", err)
	}
	return old.String, nil
}
