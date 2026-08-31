package db

import (
	"path/filepath"
	"testing"
)

// TestMemoryDisciplineSeed_OneShot mirrors TestTypedTicketSeed_OneShot: the
// niwa/tsukumo rollout seed for require_memory_discipline is applied exactly
// once, and an operator opt-out must survive a relay restart (boot-time seed
// must NOT silently re-enable the flag).
func TestMemoryDisciplineSeed_OneShot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem-seed.db")

	d1, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("boot 1: %v", err)
	}
	if !d1.ProjectRequiresMemoryDiscipline("niwa") {
		t.Fatalf("boot 1: expected niwa seeded require_memory_discipline=1")
	}
	if !d1.ProjectRequiresMemoryDiscipline("tsukumo") {
		t.Fatalf("boot 1: expected tsukumo seeded require_memory_discipline=1")
	}
	if err := d1.SetProjectRequiresMemoryDiscipline("niwa", false); err != nil {
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
	if d2.ProjectRequiresMemoryDiscipline("niwa") {
		t.Fatalf("boot 2: operator opt-out was clobbered — seed re-enabled niwa on restart")
	}
	// tsukumo's marker is independent of niwa's.
	if !d2.ProjectRequiresMemoryDiscipline("tsukumo") {
		t.Fatalf("boot 2: tsukumo seed regressed")
	}
}

// TestMemoryDisciplineSeed_UnknownProjectOff proves a project that never opted
// in — the vast majority the relay serves — reads false: an unknown project or
// a fresh one with no row read false rather than erroring.
func TestMemoryDisciplineSeed_UnknownProjectOff(t *testing.T) {
	d := soakDB(t)
	if d.ProjectRequiresMemoryDiscipline("some-random-project") {
		t.Fatal("expected an unseeded project to read require_memory_discipline=false")
	}
	d.EnsureProject("freshly-created")
	if d.ProjectRequiresMemoryDiscipline("freshly-created") {
		t.Fatal("expected a freshly created project to default to require_memory_discipline=false")
	}
}
