package db

import (
	"testing"
	"time"
)

// T5: memories with a past valid_until DEGRADE to stale — still stored and
// searchable via include_stale, hidden from default reads, never removed.
func TestMemoryTemporalValidity(t *testing.T) {
	d := testDB(t)

	if _, err := d.SetMemory("p1", "bot-a", "expiring", "soon gone", "[]", "project", "stated", "behavior"); err != nil {
		t.Fatalf("set memory: %v", err)
	}

	// Stamp a valid_until in the past — memory is now stale.
	past := time.Now().UTC().Add(-time.Hour).Format(memoryTimeFmt)
	if err := d.SetMemoryValidity("p1", "bot-a", "expiring", "project", "", past); err != nil {
		t.Fatalf("set validity: %v", err)
	}

	// Default search (live only) must hide it.
	live, err := d.SearchMemory("p1", "bot-a", "gone", nil, "project", 20)
	if err != nil {
		t.Fatalf("search live: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("stale memory leaked into default search: got %d", len(live))
	}

	// include_stale must return it, flagged stale — never removed.
	stale, err := d.SearchMemory("p1", "bot-a", "gone", nil, "project", 20, true)
	if err != nil {
		t.Fatalf("search stale: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("include_stale should return the stale memory: got %d", len(stale))
	}
	if stale[0].Status != "stale" {
		t.Errorf("expected status=stale, got %q", stale[0].Status)
	}

	// list_memories mirrors the same live-only default.
	listLive, _ := d.ListMemories("p1", "project", "", nil, 50)
	if len(listLive) != 0 {
		t.Errorf("stale memory leaked into default list: got %d", len(listLive))
	}
	listStale, _ := d.ListMemories("p1", "project", "", nil, 50, true)
	if len(listStale) != 1 || listStale[0].Status != "stale" {
		t.Errorf("include_stale list should return 1 stale memory, got %d", len(listStale))
	}

	// Direct recall by key still surfaces it (stale is stored, only flagged).
	got, _ := d.GetMemory("p1", "bot-a", "expiring", "project")
	if len(got) != 1 {
		t.Fatalf("recall of stale key should return it, got %d", len(got))
	}
	if got[0].Status != "stale" {
		t.Errorf("recalled memory should be flagged stale, got %q", got[0].Status)
	}

	// Boot view is live-only: a stale memory must not be re-injected as canon.
	boot, _ := d.ListBootMemories("p1", "bot-a", 50)
	if len(boot) != 0 {
		t.Errorf("stale memory must not appear in boot view, got %d", len(boot))
	}
}

// T5: a caller-supplied valid_until in a non-canonical ISO-8601 form (bare 'Z',
// no microseconds) must still order correctly against `now`. Without
// normalization 'Z' (0x5A) sorts after '.' (0x2E) and an expired memory would
// wrongly read live.
func TestMemoryValidityNormalizesTimestampForm(t *testing.T) {
	d := testDB(t)
	if _, err := d.SetMemory("p1", "bot-a", "iso", "v", "[]", "project", "stated", "behavior"); err != nil {
		t.Fatalf("set: %v", err)
	}
	// RFC3339 'Z' form, one hour in the past — no microseconds.
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := d.SetMemoryValidity("p1", "bot-a", "iso", "project", "", past); err != nil {
		t.Fatalf("set validity: %v", err)
	}
	live, _ := d.ListMemories("p1", "project", "", nil, 50)
	if len(live) != 0 {
		t.Fatalf("expired (RFC3339 form) memory leaked into default list: got %d", len(live))
	}
	stale, _ := d.ListMemories("p1", "project", "", nil, 50, true)
	if len(stale) != 1 || stale[0].Status != "stale" {
		t.Fatalf("include_stale should return 1 stale memory, got %d", len(stale))
	}
	// Garbage timestamp is rejected loudly, not silently stored.
	if err := d.SetMemoryValidity("p1", "bot-a", "iso", "project", "", "not-a-time"); err == nil {
		t.Error("expected error on unparseable valid_until, got nil")
	}
}

// T5: delete is a soft-archive with a full tombstone (who/when/why); a live read
// hides the archived key but GetMemoryIncludingArchived surfaces the tombstone.
func TestMemoryDeleteTombstone(t *testing.T) {
	d := testDB(t)
	if _, err := d.SetMemory("p1", "bot-a", "doomed", "bye", "[]", "project", "stated", "behavior"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := d.DeleteMemory("p1", "bot-a", "doomed", "project", "cleanup sweep"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Live recall is empty (archived filtered out).
	live, _ := d.GetMemory("p1", "bot-a", "doomed", "project")
	if len(live) != 0 {
		t.Errorf("archived key should not appear in live recall, got %d", len(live))
	}

	// Tombstone recall surfaces who/when/why + status.
	tomb, err := d.GetMemoryIncludingArchived("p1", "bot-a", "doomed", "project")
	if err != nil {
		t.Fatalf("archived recall: %v", err)
	}
	if len(tomb) != 1 {
		t.Fatalf("expected 1 archived memory, got %d", len(tomb))
	}
	m := tomb[0]
	if m.Status != "archived" {
		t.Errorf("status: want archived, got %q", m.Status)
	}
	if m.ArchivedAt == nil {
		t.Error("tombstone missing archived_at (when)")
	}
	if m.ArchivedBy == nil || *m.ArchivedBy != "bot-a" {
		t.Errorf("tombstone who: want bot-a, got %v", m.ArchivedBy)
	}
	if m.ArchivedReason == nil || *m.ArchivedReason != "cleanup sweep" {
		t.Errorf("tombstone why: want 'cleanup sweep', got %v", m.ArchivedReason)
	}
}

// T5: freshly set memories are backfilled/stamped live with a valid_from.
func TestMemorySetStampsValidFromLive(t *testing.T) {
	d := testDB(t)
	m, err := d.SetMemory("p1", "bot-a", "k", "v", "[]", "project", "stated", "behavior")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if m.Status != "live" {
		t.Errorf("fresh memory status: want live, got %q", m.Status)
	}
	if m.ValidFrom == nil || *m.ValidFrom == "" {
		t.Error("fresh memory should have valid_from stamped")
	}
	got, _ := d.GetMemory("p1", "bot-a", "k", "project")
	if len(got) != 1 || got[0].ValidFrom == nil {
		t.Error("stored memory should carry valid_from on read")
	}
}
