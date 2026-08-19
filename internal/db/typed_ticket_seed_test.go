package db

import (
	"path/filepath"
	"testing"
)

// TestTypedTicketSeed_OneShot proves the niwa rollout seed is applied exactly
// once (F1). An operator who opts niwa OUT after the seed must survive a relay
// restart — the boot-time seed must NOT silently re-enable the flag.
func TestTypedTicketSeed_OneShot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.db")

	// Boot 1: fresh DB — niwa seeded on by the rollout default.
	d1, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("boot 1: %v", err)
	}
	if !d1.ProjectRequiresTypedTicket("niwa") {
		t.Fatalf("boot 1: expected niwa seeded require_typed_ticket=1")
	}
	// Operator deliberately opts niwa out.
	if err := d1.SetProjectRequiresTypedTicket("niwa", false); err != nil {
		t.Fatalf("opt-out: %v", err)
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	// Boot 2: same DB file — simulates a relay restart re-running migrate().
	d2, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	if d2.ProjectRequiresTypedTicket("niwa") {
		t.Fatalf("boot 2: operator opt-out was clobbered — seed re-enabled niwa on restart")
	}
}

// TestTypedTicketSeed_Tsukumo proves tsukumo (the fleet's own project) is seeded
// on so the b092a6be guard actually fires for us — G1 in the lifecycle audit was
// that the guard existed but tsukumo was default-off. The one-shot marker is
// per-project: opting tsukumo out must survive a restart independently of niwa.
func TestTypedTicketSeed_Tsukumo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed-tsukumo.db")

	d1, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("boot 1: %v", err)
	}
	if !d1.ProjectRequiresTypedTicket("tsukumo") {
		t.Fatalf("boot 1: expected tsukumo seeded require_typed_ticket=1")
	}
	// niwa still seeded independently by its own marker.
	if !d1.ProjectRequiresTypedTicket("niwa") {
		t.Fatalf("boot 1: niwa seed regressed")
	}
	if err := d1.SetProjectRequiresTypedTicket("tsukumo", false); err != nil {
		t.Fatalf("opt-out: %v", err)
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	d2, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	if d2.ProjectRequiresTypedTicket("tsukumo") {
		t.Fatalf("boot 2: operator opt-out was clobbered — seed re-enabled tsukumo on restart")
	}
}
