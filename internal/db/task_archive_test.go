package db

import (
	"testing"
)

// countArchivedAudit returns how many "task.archived" audit rows exist for a task.
func countArchivedAudit(t *testing.T, d *DB, project, taskID string) int {
	t.Helper()
	entries, err := d.ListAudit(project, taskID, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == "task.archived" {
			n++
		}
	}
	return n
}

// TestArchiveTaskStampsAndAuditsOnce: a first archive stamps archived_at and
// writes EXACTLY one "task.archived" audit row carrying the reason and actor.
func TestArchiveTaskStampsAndAuditsOnce(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("proj", "worker", "cto", "t1", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	ok, err := d.ArchiveTask("proj", task.ID, "stale", "cto-tsukumo")
	if err != nil || !ok {
		t.Fatalf("archive: ok=%v err=%v", ok, err)
	}

	got, err := d.GetTask(task.ID, "proj")
	if err != nil || got == nil {
		t.Fatalf("get task: %v (nil=%v)", err, got == nil)
	}
	if got.ArchivedAt == nil {
		t.Fatal("archived_at must be stamped")
	}
	if n := countArchivedAudit(t, d, "proj", task.ID); n != 1 {
		t.Fatalf("want exactly 1 task.archived audit row, got %d", n)
	}
	entries, _ := d.ListAudit("proj", task.ID, 100)
	for _, e := range entries {
		if e.Action == "task.archived" {
			if e.Reason != "stale" || e.Actor != "cto-tsukumo" {
				t.Fatalf("audit row reason/actor = %q/%q, want stale/cto-tsukumo", e.Reason, e.Actor)
			}
		}
	}
}

// TestArchiveTaskSecondCallNoop: archiving an already-archived task returns
// (false, nil), does NOT re-stamp, and writes NO second audit row.
func TestArchiveTaskSecondCallNoop(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("proj", "worker", "cto", "t1", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if ok, err := d.ArchiveTask("proj", task.ID, "stale", "cto-tsukumo"); err != nil || !ok {
		t.Fatalf("first archive: ok=%v err=%v", ok, err)
	}
	first, _ := d.GetTask(task.ID, "proj")

	ok, err := d.ArchiveTask("proj", task.ID, "again", "someone-else")
	if err != nil {
		t.Fatalf("second archive err: %v", err)
	}
	if ok {
		t.Fatal("second archive must return false (already archived)")
	}
	second, _ := d.GetTask(task.ID, "proj")
	if first.ArchivedAt == nil || second.ArchivedAt == nil || *first.ArchivedAt != *second.ArchivedAt {
		t.Fatal("archived_at must not change on the second call")
	}
	if n := countArchivedAudit(t, d, "proj", task.ID); n != 1 {
		t.Fatalf("want still exactly 1 audit row, got %d", n)
	}

	// A missing id is likewise a no-op with no audit.
	if ok, err := d.ArchiveTask("proj", "does-not-exist", "stale", "cto"); err != nil || ok {
		t.Fatalf("missing id: want (false,nil), got ok=%v err=%v", ok, err)
	}
	if n := countArchivedAudit(t, d, "proj", "does-not-exist"); n != 0 {
		t.Fatalf("missing id must write no audit, got %d", n)
	}
}

// TestArchiveTaskEmptyReasonRefused: an empty reason errors BEFORE any write —
// the task stays active and no audit row is written.
func TestArchiveTaskEmptyReasonRefused(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("proj", "worker", "cto", "t1", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	ok, err := d.ArchiveTask("proj", task.ID, "", "cto-tsukumo")
	if err == nil {
		t.Fatal("empty reason must return an error")
	}
	if ok {
		t.Fatal("empty reason must return false")
	}
	got, _ := d.GetTask(task.ID, "proj")
	if got == nil || got.ArchivedAt != nil {
		t.Fatal("task must remain active when reason is empty")
	}
	if n := countArchivedAudit(t, d, "proj", task.ID); n != 0 {
		t.Fatalf("empty reason must write no audit, got %d", n)
	}
}
