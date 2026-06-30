package db

import (
	"path/filepath"
	"testing"
)

// TestMetricsSnapshotDeadLettered covers the TSU-147 gauge: dead-lettered
// events (those the outbox gave up on) are counted; live/retrying ones are not.
func TestMetricsSnapshotDeadLettered(t *testing.T) {
	database, err := NewTestDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// One event that retries then dead-letters, one that's just retrying.
	deadID, _, err := database.InsertEvent("", "default", "event:x", "a", `{}`)
	if err != nil {
		t.Fatalf("insert dead event: %v", err)
	}
	liveID, _, err := database.InsertEvent("", "default", "event:y", "a", `{}`)
	if err != nil {
		t.Fatalf("insert live event: %v", err)
	}
	if err := database.MarkEventDead(deadID, "DLQ: all matched rules failed"); err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	if err := database.IncrementEventAttempt(liveID, "transient"); err != nil {
		t.Fatalf("increment attempt: %v", err)
	}

	m := database.MetricsSnapshot()
	if m.EventsDeadLettered != 1 {
		t.Errorf("EventsDeadLettered = %d, want 1 (only the DLQ'd event counts)", m.EventsDeadLettered)
	}
}
