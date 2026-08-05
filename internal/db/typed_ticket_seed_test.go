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
