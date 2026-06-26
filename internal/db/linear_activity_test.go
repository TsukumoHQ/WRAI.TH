package db

import "testing"

// TestUpsertLinearMirror_BumpsActivityOnTransition guards the dispatch-flip
// calibration bug: when a C-level moves a Linear issue Todo→In Progress (the
// dispatch), the mirror must bump last_activity_at — a freshly dispatched task
// is the OPPOSITE of stale. A content-only re-sync (same status) must NOT bump.
func TestUpsertLinearMirror_BumpsActivityOnTransition(t *testing.T) {
	d := testDB(t)
	const project = "p1"
	const old = "2020-01-01T00:00:00.000000Z"

	// Mirror a pending (Todo) issue.
	taskID, created, err := d.UpsertLinearMirror(LinearMirrorSeed{
		Project: project, LinearIssueID: "iss-1", Title: "badge", Status: "pending",
	})
	if err != nil || !created {
		t.Fatalf("insert: created=%v err=%v", created, err)
	}
	// Backdate last_activity_at to simulate a task dispatched hours after creation.
	if _, err := d.conn.Exec("UPDATE tasks SET last_activity_at=? WHERE id=?", old, taskID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Dispatch: Todo → In Progress (status changes) → must bump.
	if _, _, err := d.UpsertLinearMirror(LinearMirrorSeed{
		Project: project, LinearIssueID: "iss-1", Title: "badge", Status: "in-progress",
	}); err != nil {
		t.Fatalf("dispatch upsert: %v", err)
	}
	got, _ := d.GetTask(taskID, project)
	if got.LastActivityAt == nil || *got.LastActivityAt == old {
		t.Fatalf("dispatch transition must bump last_activity_at off %s, got %v", old, got.LastActivityAt)
	}

	// Backdate again, then a content-only re-sync (SAME status) must NOT bump.
	if _, err := d.conn.Exec("UPDATE tasks SET last_activity_at=? WHERE id=?", old, taskID); err != nil {
		t.Fatalf("backdate 2: %v", err)
	}
	if _, _, err := d.UpsertLinearMirror(LinearMirrorSeed{
		Project: project, LinearIssueID: "iss-1", Title: "badge (edited title)", Status: "in-progress",
	}); err != nil {
		t.Fatalf("resync upsert: %v", err)
	}
	got, _ = d.GetTask(taskID, project)
	if got.LastActivityAt == nil || *got.LastActivityAt != old {
		t.Fatalf("content-only re-sync must NOT bump last_activity_at (want %s, got %v)", old, got.LastActivityAt)
	}
}
