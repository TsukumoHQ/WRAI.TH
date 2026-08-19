package db

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestBackupRestoreDrill seeds fleet state, snapshots it, then runs the verified
// restore drill: the snapshot must pass integrity + FK checks and its core-table
// row counts must match the source. A corrupt file must be rejected.
func TestBackupRestoreDrill(t *testing.T) {
	d := testDB(t)

	for i := 0; i < 3; i++ {
		if _, _, err := d.RegisterAgent("default", fmt.Sprintf("a%d", i), "drill", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register a%d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := d.InsertMessageWithDeliveries("default", "a0", "a1", "notification", "s", "m", "{}", "P2", 3600, nil, nil, []string{"a1"}, ""); err != nil {
			t.Fatalf("insert msg %d: %v", i, err)
		}
	}
	if err := d.RecordAudit(auditEntry("default", "a0", "transition", "t1", "seed")); err != nil {
		t.Fatalf("record audit: %v", err)
	}

	// Live DB is sound.
	if err := d.IntegrityCheck(); err != nil {
		t.Fatalf("live IntegrityCheck: %v", err)
	}

	snap, err := d.Backup(2)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	counts, err := VerifyDBFile(snap)
	if err != nil {
		t.Fatalf("restore drill failed on a good snapshot: %v", err)
	}
	want := map[string]int64{"agents": 3, "messages": 5, "deliveries": 5, "audit_log": 1}
	for tbl, n := range want {
		if counts[tbl] != n {
			t.Errorf("snapshot %s count = %d, want %d (restore would lose/gain rows)", tbl, counts[tbl], n)
		}
	}

	// Corruption / non-DB file must be rejected, not silently "verified".
	bad := filepath.Join(t.TempDir(), "bad.db")
	if err := os.WriteFile(bad, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write bad file: %v", err)
	}
	if _, err := VerifyDBFile(bad); err == nil {
		t.Error("VerifyDBFile accepted a non-database file")
	}

	// A missing snapshot path is an error, not a panic.
	if _, err := VerifyDBFile(filepath.Join(t.TempDir(), "nope.db")); err == nil {
		t.Error("VerifyDBFile accepted a missing path")
	}
}
