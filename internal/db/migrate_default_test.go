package db

import (
	"path/filepath"
	"testing"
	"time"
)

// TestMigratePurgeDefaultProject verifies the retired 'default' catch-all and
// all rows attributed to it are removed, real projects survive untouched, and a
// second pass is a no-op.
func TestMigratePurgeDefaultProject(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now().UTC().Format(time.RFC3339)
	seed := []struct {
		q    string
		args []any
	}{
		// 'default' project + its data — must all be purged.
		{"INSERT INTO projects (name, planet_type, created_at) VALUES ('default','forest/1',?)", []any{now}},
		{"INSERT INTO agents (id, name, role, registered_at, last_seen, project) VALUES ('a1','bot','',?,?,'default')", []any{now, now}},
		{"INSERT INTO messages (id, from_agent, to_agent, type, subject, content, created_at, project) VALUES ('m1','bot','user','notification','','x',?,'default')", []any{now}},
		// A delivery for m1: deliveries.message_id -> messages(id) is the ONE
		// enforced FK (RESTRICT). Purging messages before this child row triggers
		// "FOREIGN KEY constraint failed" — the 'mauvais waterfall' regression.
		{"INSERT INTO deliveries (id, message_id, to_agent, created_at, project) VALUES ('d1','m1','user',?,'default')", []any{now}},
		// A memory so the FTS-consistency path is exercised (memories_fts is
		// content-synced via triggers; the purge must leave no dangling index row).
		{"INSERT INTO memories (id, key, value, scope, project, agent_name, created_at, updated_at) VALUES ('mem1','k','searchme','project','default','bot',?,?)", []any{now, now}},
		// A real project + its data — must survive.
		{"INSERT INTO projects (name, planet_type, created_at) VALUES ('trovex-growth','forest/1',?)", []any{now}},
		{"INSERT INTO agents (id, name, role, registered_at, last_seen, project) VALUES ('a2','cmo','',?,?,'trovex-growth')", []any{now, now}},
		{"INSERT INTO messages (id, from_agent, to_agent, type, subject, content, created_at, project) VALUES ('m2','cmo','user','notification','','y',?,'trovex-growth')", []any{now}},
		// A GLOBAL notification rule (project='default' = wildcard, not catch-all)
		// — must survive: purging it would silently disable global notifications.
		{"INSERT INTO notification_rules (id, project, name, event, action, created_at, updated_at) VALUES ('r1','default','global-rule','task.dispatched','log',?,?)", []any{now, now}},
	}
	for _, s := range seed {
		if _, err := d.conn.Exec(s.q, s.args...); err != nil {
			t.Fatalf("seed %q: %v", s.q, err)
		}
	}

	migratePurgeDefaultProject(d.conn)

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
	// default gone everywhere.
	assertOne("SELECT COUNT(*) FROM projects WHERE name = 'default'", 0)
	assertOne("SELECT COUNT(*) FROM agents WHERE project = 'default'", 0)
	assertOne("SELECT COUNT(*) FROM messages WHERE project = 'default'", 0)
	// FK child (deliveries) purged before its parent (messages) — with the
	// waterfall bug the messages DELETE would abort the tx and leave m1 behind,
	// failing the messages assert above.
	assertOne("SELECT COUNT(*) FROM deliveries WHERE project = 'default'", 0)
	assertOne("SELECT COUNT(*) FROM deliveries WHERE message_id = 'm1'", 0)
	// memory purged and its FTS shadow left with no dangling row.
	assertOne("SELECT COUNT(*) FROM memories WHERE project = 'default'", 0)
	assertOne("SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH 'searchme'", 0)
	// real project intact.
	assertOne("SELECT COUNT(*) FROM projects WHERE name = 'trovex-growth'", 1)
	assertOne("SELECT COUNT(*) FROM agents WHERE project = 'trovex-growth'", 1)
	assertOne("SELECT COUNT(*) FROM messages WHERE project = 'trovex-growth'", 1)
	// global notification rule survives — 'default' there is the wildcard, not the catch-all.
	assertOne("SELECT COUNT(*) FROM notification_rules WHERE project = 'default'", 1)

	// Idempotent: a second pass changes nothing and does not error.
	migratePurgeDefaultProject(d.conn)
	assertOne("SELECT COUNT(*) FROM projects WHERE name = 'trovex-growth'", 1)
}
