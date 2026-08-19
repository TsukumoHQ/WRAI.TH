package db

import "testing"

// DeleteTask must clean its OWNED child rows (task_progress_notes) so no logical
// orphan survives, while leaving soft-linked entities alone: a subtask
// (parent_task_id) must NOT be cascade-deleted.
func TestDeleteTask_CleansProgressNotes_KeepsSubtasks(t *testing.T) {
	d := testDB(t)

	parent, err := d.DispatchTask("p1", "dev", "cto", "parent", "", "P2", nil, nil, TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch parent: %v", err)
	}
	if err := d.AddProgressNote(parent.ID, "p1", "cto", "note1"); err != nil {
		t.Fatalf("note1: %v", err)
	}
	if err := d.AddProgressNote(parent.ID, "p1", "cto", "note2"); err != nil {
		t.Fatalf("note2: %v", err)
	}
	sub, err := d.DispatchTask("p1", "dev", "cto", "child", "", "P2", &parent.ID, nil, TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch subtask: %v", err)
	}

	// sanity: the notes exist before delete.
	if notes, _ := d.GetProgressNotes(parent.ID, "p1"); len(notes) != 2 {
		t.Fatalf("expected 2 progress notes pre-delete, got %d", len(notes))
	}

	if err := d.DeleteTask(parent.ID, "p1"); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	// owned child rows gone (no orphan).
	if notes, _ := d.GetProgressNotes(parent.ID, "p1"); len(notes) != 0 {
		t.Errorf("progress notes must be cleaned on task delete, got %d orphans", len(notes))
	}
	// belt: nothing lingers in the table for that task id.
	var n int
	if err := d.ro().QueryRow("SELECT COUNT(*) FROM task_progress_notes WHERE task_id = ?", parent.ID).Scan(&n); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if n != 0 {
		t.Errorf("orphaned task_progress_notes remain: %d", n)
	}
	// the task itself is gone.
	if got, _ := d.GetTask(parent.ID, "p1"); got != nil {
		t.Error("parent task must be deleted")
	}
	// the subtask is a soft link — it must SURVIVE (no silent cascade).
	if got, _ := d.GetTask(sub.ID, "p1"); got == nil {
		t.Error("subtask must not be cascade-deleted with its parent")
	}
}

// DeleteAgent is a deliberate tombstone: the row stays with status='deleted' so
// dependents reference a present-but-tombstoned agent (no logical orphan), and a
// re-register revives it.
func TestDeleteAgent_TombstonesRow(t *testing.T) {
	d := testDB(t)
	if _, _, err := d.RegisterAgent("p1", "bot", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := d.DeleteAgent("p1", "bot"); err != nil {
		t.Fatalf("delete agent: %v", err)
	}

	// the row is KEPT, flipped to 'deleted' (queried directly — the row must exist).
	var status string
	if err := d.ro().QueryRow("SELECT status FROM agents WHERE name = ? AND project = ?", "bot", "p1").Scan(&status); err != nil {
		t.Fatalf("tombstone row must still exist: %v", err)
	}
	if status != "deleted" {
		t.Errorf("expected status 'deleted', got %q", status)
	}

	// re-register revives the same name back to active (history intact).
	a, _, err := d.RegisterAgent("p1", "bot", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if a.Status != "active" {
		t.Errorf("re-register must revive to active, got %q", a.Status)
	}
}
