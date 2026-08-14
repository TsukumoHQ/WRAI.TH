package db

import (
	"path/filepath"
	"testing"
	"time"
)

// TestMigrateNormalizeProjects reproduces the upgrade hazard: a DB written by
// a pre-normalization relay ("testDuSoir", "synergix_prod@synx-prod") opened
// by the new binary must come out canonical — including merging a collision
// where both spellings exist — while internal "_" names stay untouched.
func TestMigrateNormalizeProjects(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now().UTC().Format(time.RFC3339)
	seed := []struct {
		q    string
		args []any
	}{
		// Old spellings.
		{"INSERT INTO projects (name, planet_type, created_at) VALUES (?, ?, ?)", []any{"testDuSoir", "ice/1", now}},
		{"INSERT INTO agents (id, name, role, registered_at, last_seen, project) VALUES ('a1','bot','', ?, ?, 'testDuSoir')", []any{now, now}},
		{"INSERT INTO messages (id, from_agent, to_agent, type, subject, content, created_at, project) VALUES ('m1','bot','user','notification','','x', ?, 'synergix_prod@synx-prod')", []any{now}},
		// Collision: canonical AND non-canonical projects rows both exist.
		{"INSERT INTO projects (name, planet_type, created_at) VALUES (?, ?, ?)", []any{"synergix-prod", "ice/1", now}},
		{"INSERT INTO projects (name, planet_type, created_at) VALUES (?, ?, ?)", []any{"synergix_prod", "ice/1", now}},
		// Internal pseudo-project must survive untouched.
		{"INSERT INTO messages (id, from_agent, to_agent, type, subject, content, created_at, project) VALUES ('m2','sys','sys','notification','','y', ?, '_relay')", []any{now}},
	}
	for _, s := range seed {
		if _, err := d.conn.Exec(s.q, s.args...); err != nil {
			t.Fatalf("seed %q: %v", s.q, err)
		}
	}

	migrateNormalizeProjects(d.conn)

	assertOne := func(q string, want int) {
		t.Helper()
		var n int
		if err := d.conn.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	assertOne("SELECT COUNT(*) FROM projects WHERE name = 'testdusoir'", 1)
	assertOne("SELECT COUNT(*) FROM projects WHERE name = 'testDuSoir'", 0)
	assertOne("SELECT COUNT(*) FROM agents WHERE project = 'testdusoir'", 1)
	assertOne("SELECT COUNT(*) FROM messages WHERE project = 'synergix-prod@synx-prod'", 1)
	// Collision merged: exactly one canonical projects row, dupe gone.
	assertOne("SELECT COUNT(*) FROM projects WHERE name = 'synergix-prod'", 1)
	assertOne("SELECT COUNT(*) FROM projects WHERE name = 'synergix_prod'", 0)
	// Internal name untouched.
	assertOne("SELECT COUNT(*) FROM messages WHERE project = '_relay'", 1)

	// Idempotent: a second pass changes nothing and does not error.
	migrateNormalizeProjects(d.conn)
	assertOne("SELECT COUNT(*) FROM projects WHERE name = 'testdusoir'", 1)
}
