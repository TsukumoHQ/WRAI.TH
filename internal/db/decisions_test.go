package db

import (
	"encoding/json"
	"testing"
)

// TestRememberDecision covers TSU-51 slice-A: ADR-style decisions stored as
// project memories, monotonic DEC keys, dedup-or-supersede, and recall of the
// accepted (non-superseded) set.
func TestRememberDecision(t *testing.T) {
	d := testDB(t)
	const project = "p1"

	m1, err := d.RememberDecision(project, "wraith-dev", "ingest/hooks",
		"POST hook events to the relay; no file-drop watcher", "watcher deadlocked the detector", []string{"arch"}, "")
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m1.Key != "DEC-ingest-hooks-1" {
		t.Fatalf("key = %q, want DEC-ingest-hooks-1", m1.Key)
	}

	// A second decision in the same area gets the next sequence number.
	m2, err := d.RememberDecision(project, "wraith-dev", "ingest/hooks",
		"tokens come from the Stop hook reading the transcript", "", nil, "")
	if err != nil {
		t.Fatalf("remember 2: %v", err)
	}
	if m2.Key != "DEC-ingest-hooks-2" {
		t.Fatalf("key = %q, want DEC-ingest-hooks-2", m2.Key)
	}

	// Dedup: re-asserting the same decision text in the area without supersedes is rejected.
	if _, err := d.RememberDecision(project, "wraith-dev", "ingest/hooks",
		"POST hook events to the relay; no file-drop watcher", "", nil, ""); err == nil {
		t.Fatal("expected near-duplicate to be rejected without supersedes")
	}

	// Recall: 2 accepted decisions.
	accepted, err := d.ListDecisions(project)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accepted) != 2 {
		t.Fatalf("want 2 accepted, got %d", len(accepted))
	}

	// Supersede m1 with a revised decision → m1 archived, accepted set still has 2
	// (m2 + the new one), m1 gone.
	m3, err := d.RememberDecision(project, "wraith-dev", "ingest/hooks",
		"POST hook events to the relay over HTTP/2", "perf", nil, m1.Key)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	var dv DecisionValue
	if err := json.Unmarshal([]byte(m3.Value), &dv); err != nil || dv.Supersedes != m1.Key {
		t.Fatalf("new decision must record supersedes=%s, got %+v (err %v)", m1.Key, dv, err)
	}
	accepted, _ = d.ListDecisions(project)
	if len(accepted) != 2 {
		t.Fatalf("after supersede want 2 accepted, got %d", len(accepted))
	}
	for _, m := range accepted {
		if m.Key == m1.Key {
			t.Fatalf("superseded decision %s must not be in the accepted set", m1.Key)
		}
	}

	// Superseding a non-existent decision errors.
	if _, err := d.RememberDecision(project, "wraith-dev", "x", "y", "", nil, "DEC-nope-9"); err == nil {
		t.Fatal("expected error superseding a non-existent decision")
	}
}
