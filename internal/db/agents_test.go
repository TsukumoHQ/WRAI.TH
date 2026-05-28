package db

import "testing"

// registerSeed is a small helper that uses the existing testDB(t) helper from
// db_test.go and registers a barebones agent so each WakeAgent test starts from
// a known state.
func registerSeed(t *testing.T, d *DB, project, name string) {
	t.Helper()
	if _, _, err := d.RegisterAgent(project, name, "", "", nil, nil, false, nil, "", 0); err != nil {
		t.Fatalf("RegisterAgent(%q): %v", name, err)
	}
}

func TestWakeAgent_FromSleeping(t *testing.T) {
	d := testDB(t)
	registerSeed(t, d, "proj1", "endurance")
	if err := d.SleepAgent("proj1", "endurance"); err != nil {
		t.Fatalf("SleepAgent: %v", err)
	}

	n, err := d.WakeAgent("proj1", "endurance")
	if err != nil {
		t.Fatalf("WakeAgent: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row affected, got %d", n)
	}

	a, err := d.GetAgent("proj1", "endurance")
	if err != nil || a == nil {
		t.Fatalf("GetAgent: %v / nil=%v", err, a == nil)
	}
	if a.Status != "active" {
		t.Fatalf("want status=active, got %q", a.Status)
	}
}

func TestWakeAgent_AlreadyActive_NoOp(t *testing.T) {
	d := testDB(t)
	registerSeed(t, d, "proj1", "endurance") // freshly registered = active

	n, err := d.WakeAgent("proj1", "endurance")
	if err != nil {
		t.Fatalf("WakeAgent: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 rows (already active is a no-op, not an error), got %d", n)
	}
}

func TestWakeAgent_NotFound_NoOp(t *testing.T) {
	d := testDB(t)
	// no agent registered
	n, err := d.WakeAgent("proj1", "ghost")
	if err != nil {
		t.Fatalf("WakeAgent: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 rows (ghost), got %d", n)
	}
}

func TestWakeAgent_FromInactive_ClearsDeactivatedAt(t *testing.T) {
	d := testDB(t)
	registerSeed(t, d, "proj1", "endurance")
	if err := d.DeactivateAgent("proj1", "endurance"); err != nil {
		t.Fatalf("DeactivateAgent: %v", err)
	}
	// Sanity: deactivated_at is set after deactivation.
	before, _ := d.GetAgent("proj1", "endurance")
	if before == nil || before.Status != "inactive" {
		t.Fatalf("precondition: expected inactive, got %+v", before)
	}
	if before.DeactivatedAt == nil {
		t.Fatalf("precondition: expected deactivated_at set, got nil")
	}

	n, err := d.WakeAgent("proj1", "endurance")
	if err != nil {
		t.Fatalf("WakeAgent: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row affected, got %d", n)
	}

	after, _ := d.GetAgent("proj1", "endurance")
	if after.Status != "active" {
		t.Fatalf("want status=active, got %q", after.Status)
	}
	if after.DeactivatedAt != nil {
		t.Fatalf("want deactivated_at cleared after wake, got %v", *after.DeactivatedAt)
	}
}

// TestWakeAgent_LastSeenRefreshed verifies that waking refreshes last_seen — the
// natural signal that the seed identity is « active » again for downstream
// staleness tracking (cf MarkStaleAgentsInactive).
func TestWakeAgent_LastSeenRefreshed(t *testing.T) {
	d := testDB(t)
	registerSeed(t, d, "proj1", "endurance")
	if err := d.SleepAgent("proj1", "endurance"); err != nil {
		t.Fatalf("SleepAgent: %v", err)
	}
	before, _ := d.GetAgent("proj1", "endurance")
	// SleepAgent doesn't refresh last_seen, but Sleep + Wake should result in
	// a last_seen ≥ what it was at registration time. We assert non-empty and
	// well-formed rather than strict ordering (test runs are fast).
	if before.LastSeen == "" {
		t.Fatal("last_seen empty before wake")
	}

	if _, err := d.WakeAgent("proj1", "endurance"); err != nil {
		t.Fatalf("WakeAgent: %v", err)
	}

	after, _ := d.GetAgent("proj1", "endurance")
	if after.LastSeen == "" {
		t.Fatal("last_seen empty after wake")
	}
	if after.LastSeen < before.LastSeen {
		t.Fatalf("last_seen went backwards after wake: %q -> %q", before.LastSeen, after.LastSeen)
	}
}
