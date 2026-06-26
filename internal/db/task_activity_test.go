package db

import "testing"

// TestLastActivityAt_BumpsOnActivity guards the stale-calibration fix: a task's
// last_activity_at is stamped at dispatch and reset by activity (transition,
// progress note) so the stale-scanner measures idle-since-activity, not
// idle-since-dispatch.
func TestLastActivityAt_BumpsOnActivity(t *testing.T) {
	d := testDB(t)
	const project = "p1"

	task, err := d.DispatchTask(project, "", "dispatcher", "build me", "", "P1", nil, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, _ := d.GetTask(task.ID, project)
	if got.LastActivityAt == nil || *got.LastActivityAt == "" {
		t.Fatalf("last_activity_at must be stamped at dispatch")
	}
	atDispatch := *got.LastActivityAt

	// A transition is activity → bumps last_activity_at.
	if _, err := d.ClaimTask(task.ID, "agent-x", project); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, _ = d.GetTask(task.ID, project)
	if got.LastActivityAt == nil || *got.LastActivityAt < atDispatch {
		t.Fatalf("transition must not regress last_activity_at (was %q, now %v)", atDispatch, got.LastActivityAt)
	}
	afterClaim := *got.LastActivityAt

	// A progress note is activity → bumps it again.
	if err := d.AddProgressNote(task.ID, project, "agent-x", "still working, big build"); err != nil {
		t.Fatalf("progress note: %v", err)
	}
	got, _ = d.GetTask(task.ID, project)
	if got.LastActivityAt == nil || *got.LastActivityAt < afterClaim {
		t.Fatalf("progress note must bump last_activity_at (was %q, now %v)", afterClaim, got.LastActivityAt)
	}
}
