package db

import (
	"testing"
	"time"
)

// TestGetAgent_NotBlockedByHeldWriteLock pins the identity-check read path
// (relay toolset.go identity gate -> db.GetAgent) against the single writer:
// with the writer holding SQLite's write lock on an uncommitted write for far
// longer than readerTimeout, GetAgent still answers from the WAL reader pool
// with the last committed row. Task 33dab33d: the field 'identity check failed:
// get agent: context deadline exceeded' is therefore NOT the write lock — reads
// and writes deadline together only when the host stalls the process.
func TestGetAgent_NotBlockedByHeldWriteLock(t *testing.T) {
	d := soakDB(t)
	if _, _, err := d.RegisterAgent("p", "a1", "role", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	orig := readerTimeout
	readerTimeout = 200 * time.Millisecond
	defer func() { readerTimeout = orig }()

	tx, err := d.conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	// A real write: takes SQLite's RESERVED lock and dirties the agents page.
	if _, err := tx.Exec("UPDATE agents SET role = 'mid-write' WHERE name = 'a1' AND project = 'p'"); err != nil {
		t.Fatalf("write under tx: %v", err)
	}

	start := time.Now()
	a, err := d.GetAgent("p", "a1")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("GetAgent under a held write lock: %v (took %s)", err, took)
	}
	if a == nil || a.Role != "role" {
		t.Fatalf("GetAgent must return the last committed row, got %+v", a)
	}
	if took > 100*time.Millisecond {
		t.Fatalf("GetAgent waited %s on the held write lock", took)
	}
}
