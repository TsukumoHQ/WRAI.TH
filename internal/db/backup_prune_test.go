package db

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// touchBackup writes a stub file next to the DB and stamps its mtime, standing in
// for a full-DB backup copy (PruneStaleBackups only inspects names + mtimes).
func touchBackup(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// TestPruneStaleBackups asserts the data-dir GC drops superseded numbered
// snapshots (>= keep) and aged one-off updater/ops backups, while NEVER touching
// the live DBs, their -wal/-shm, the kept snapshots, the newest one-off (rollback
// point), or an unrelated file that merely shares the relay.db prefix.
func TestPruneStaleBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	d, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	// Live sidecars that must survive.
	touchBackup(t, dir, "relay.db-wal", 0)
	touchBackup(t, dir, "relay.db-shm", 0)
	analytics := analyticsDBPath(path) // relay.analytics.db (created by migrate)

	// Numbered snapshots 0..5; keep=3 must retain 0,1,2 and drop 3,4,5.
	for i := 0; i <= 5; i++ {
		touchBackup(t, dir, "relay.db.bak."+itoa(i), time.Duration(i)*time.Minute)
	}

	// One-off updater/ops backups. recentPreupg is the newest → kept as rollback.
	recent := touchBackup(t, dir, "relay.db.bak-preupg", 10*time.Minute)
	aged := []string{
		touchBackup(t, dir, "relay.db.PRE-RESET", 30*time.Hour),
		touchBackup(t, dir, "relay.db.v115-broken", 40*time.Hour),
		touchBackup(t, dir, "relay.analytics.db.bak-preupg", 50*time.Hour),
	}
	// An unrelated file sharing the prefix but with no backup marker: must survive.
	notes := touchBackup(t, dir, "relay.db.notes", 72*time.Hour)

	removed, err := d.PruneStaleBackups(3, time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	// Live DBs + sidecars survive.
	for _, p := range []string{path, path + "-wal", path + "-shm", analytics} {
		if !exists(p) {
			t.Errorf("live file removed: %s", p)
		}
	}
	// Kept snapshots survive; superseded ones gone.
	for i := 0; i <= 2; i++ {
		if !exists(filepath.Join(dir, "relay.db.bak."+itoa(i))) {
			t.Errorf("kept snapshot bak.%d was removed", i)
		}
	}
	for i := 3; i <= 5; i++ {
		if exists(filepath.Join(dir, "relay.db.bak."+itoa(i))) {
			t.Errorf("superseded snapshot bak.%d not removed", i)
		}
	}
	// Newest one-off kept; aged ones pruned.
	if !exists(recent) {
		t.Errorf("newest one-off backup (rollback point) was removed")
	}
	for _, p := range aged {
		if exists(p) {
			t.Errorf("aged one-off backup not pruned: %s", p)
		}
	}
	// Unrelated file survives.
	if !exists(notes) {
		t.Errorf("non-backup file relay.db.notes was removed")
	}

	// Idempotent: a second run removes nothing (everything left is protected/kept).
	removed2, err := d.PruneStaleBackups(3, time.Hour)
	if err != nil {
		t.Fatalf("prune 2: %v", err)
	}
	if len(removed2) != 0 {
		t.Errorf("prune not idempotent: second run removed %v", removed2)
	}
	_ = removed
}

// TestPruneKeepsNewestWhenAllAged asserts the newest one-off backup is retained
// as the last known-good rollback point even when every one-off is older than
// minForeignAge — retention never leaves zero recovery points.
func TestPruneKeepsNewestWhenAllAged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	d, err := NewTestDB(path)
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	newest := touchBackup(t, dir, "relay.db.bak-preupg", 25*time.Hour)
	older := touchBackup(t, dir, "relay.db.v115-broken", 100*time.Hour)

	removed, err := d.PruneStaleBackups(3, time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !exists(newest) {
		t.Errorf("newest one-off must be kept even when aged")
	}
	if exists(older) {
		t.Errorf("older one-off should be pruned")
	}
	sort.Strings(removed)
	if len(removed) != 1 || removed[0] != "relay.db.v115-broken" {
		t.Errorf("expected only relay.db.v115-broken pruned, got %v", removed)
	}
}

// itoa avoids pulling strconv into the test for single-digit slot numbers.
func itoa(i int) string { return string(rune('0' + i)) }
