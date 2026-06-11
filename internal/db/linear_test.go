package db

import (
	"path/filepath"
	"testing"
)

func mirrorTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := NewTestDB(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestUpsertLinearMirror_InsertThenUpdate(t *testing.T) {
	d := mirrorTestDB(t)

	pts := 5
	id1, created, err := d.UpsertLinearMirror(LinearMirrorSeed{
		Project:       "syn",
		LinearIssueID: "iss-1",
		LinearKey:     sp("SYN-1"),
		Title:         "First",
		Status:        "in-progress",
		Points:        &pts,
		Labels:        `["a","b"]`,
		LinearState:   sp("In Progress"),
		Assignee:      sp("lead"),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on first upsert")
	}

	got, err := d.GetTaskByLinearIssueID("syn", "iss-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Source != "linear" || got.Title != "First" || got.Status != "in-progress" {
		t.Errorf("unexpected row: %+v", got)
	}

	// Stamp some overlay state, then update — overlay must survive.
	if _, err := d.conn.Exec(`UPDATE tasks SET claimed_by='lead', in_review_at='2026-01-01T00:00:00Z' WHERE id=?`, id1); err != nil {
		t.Fatal(err)
	}

	id2, created2, err := d.UpsertLinearMirror(LinearMirrorSeed{
		Project:       "syn",
		LinearIssueID: "iss-1",
		Title:         "Renamed",
		Status:        "in-review",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if created2 {
		t.Errorf("expected created=false on update")
	}
	if id1 != id2 {
		t.Errorf("task id changed: %s -> %s", id1, id2)
	}

	after, _ := d.GetTaskByLinearIssueID("syn", "iss-1")
	if after.Title != "Renamed" {
		t.Errorf("title not updated: %q", after.Title)
	}
	if after.ClaimedBy == nil || *after.ClaimedBy != "lead" {
		t.Errorf("overlay claimed_by clobbered: %v", after.ClaimedBy)
	}
	if after.InReviewAt == nil || *after.InReviewAt != "2026-01-01T00:00:00Z" {
		t.Errorf("overlay in_review_at clobbered: %v", after.InReviewAt)
	}
}

func TestMarkLinearDone(t *testing.T) {
	d := mirrorTestDB(t)
	id, _, err := d.UpsertLinearMirror(LinearMirrorSeed{Project: "syn", LinearIssueID: "iss-2", Title: "T", Status: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkLinearDone(id); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetTaskByLinearIssueID("syn", "iss-2")
	if got.DoneAt == nil || *got.DoneAt == "" {
		t.Errorf("done_at not stamped")
	}
	if got.CompletedAt == nil || *got.CompletedAt == "" {
		t.Errorf("completed_at not stamped")
	}
	first := *got.DoneAt
	// Idempotent: a second call must not overwrite the original stamp.
	if err := d.MarkLinearDone(id); err != nil {
		t.Fatal(err)
	}
	again, _ := d.GetTaskByLinearIssueID("syn", "iss-2")
	if *again.DoneAt != first {
		t.Errorf("done_at overwritten: %s -> %s", first, *again.DoneAt)
	}
}

func TestGetTaskByLinearIssueID_Missing(t *testing.T) {
	d := mirrorTestDB(t)
	got, err := d.GetTaskByLinearIssueID("syn", "nope")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing issue, got %+v", got)
	}
}

func TestLinearSyncLogCap(t *testing.T) {
	d := mirrorTestDB(t)
	// Write a couple of entries and read them back.
	d.LogLinearSync("iss-1", "in_review", "ok", "")
	d.LogLinearSync("iss-1", "comment", "error", "boom")
	entries, err := d.RecentLinearSync(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// Newest first.
	if entries[0].Action != "comment" || entries[0].Outcome != "error" {
		t.Errorf("unexpected newest entry: %+v", entries[0])
	}
}
