package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackup_VacuumIntoAndRotate(t *testing.T) {
	dir := t.TempDir()
	d, err := NewTestDB(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	// First snapshot.
	p0, err := d.Backup(3)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, err := os.Stat(p0); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}

	// A few more rotations; .bak.0 stays newest, older slots fill in.
	for i := 0; i < 3; i++ {
		if _, err := d.Backup(3); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	for _, slot := range []string{".bak.0", ".bak.1", ".bak.2"} {
		if _, err := os.Stat(filepath.Join(dir, "relay.db"+slot)); err != nil {
			t.Errorf("expected rotated snapshot %s: %v", slot, err)
		}
	}
	// keep=3 → no .bak.3
	if _, err := os.Stat(filepath.Join(dir, "relay.db.bak.3")); err == nil {
		t.Error("retained more snapshots than keep=3")
	}
}
