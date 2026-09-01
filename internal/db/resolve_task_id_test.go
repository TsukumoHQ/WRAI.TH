package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// seedLinearTask inserts a task with explicit Linear dual-key fields so the
// resolver tests can look it up by linear_key / linear_issue_id.
func seedLinearTask(t *testing.T, conn *sql.DB, id, project, title, dispatchedAt, linearKey, linearIssueID string) {
	t.Helper()
	_, err := conn.Exec(
		`INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, linear_key, linear_issue_id, labels, blocked_periods, goal, acceptance_criteria, dod)
		 VALUES (?, '', 'cto', ?, 'pending', ?, ?, ?, ?, '[]', '[]', '', '[]', '')`,
		id, title, project, dispatchedAt, nullIfEmpty(linearKey), nullIfEmpty(linearIssueID),
	)
	if err != nil {
		t.Fatalf("seed linear task %s: %v", id, err)
	}
}

func TestResolveTaskID(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	const fullID = "11111111-2222-3333-4444-555555555555"
	const issueUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	seedLinearTask(t, d.conn, fullID, "p1", "Fix the thing", "2026-01-01T00:00:00Z", "SYN-3296", issueUUID)
	// A same-key row in another project must never be returned for p1.
	seedLinearTask(t, d.conn, "99999999-0000-0000-0000-000000000000", "p2", "Other project", "2026-01-01T00:00:00Z", "SYN-3296", "")

	t.Run("linear_key resolves", func(t *testing.T) {
		got, err := d.ResolveTaskID("SYN-3296", "p1")
		if err != nil || got != fullID {
			t.Fatalf("SYN-3296 → %q, %v (want %s)", got, err, fullID)
		}
		task, err := d.GetTask(got, "p1")
		if err != nil || task.Title != "Fix the thing" {
			t.Fatalf("get_task after resolve: %+v, %v", task, err)
		}
	})

	t.Run("linear_issue_id resolves", func(t *testing.T) {
		got, err := d.ResolveTaskID(issueUUID, "p1")
		if err != nil || got != fullID {
			t.Fatalf("issue UUID → %q, %v (want %s)", got, err, fullID)
		}
	})

	t.Run("full UUID unchanged", func(t *testing.T) {
		got, err := d.ResolveTaskID(fullID, "p1")
		if err != nil || got != fullID {
			t.Fatalf("full id → %q, %v", got, err)
		}
	})

	t.Run("UUID prefix unchanged", func(t *testing.T) {
		got, err := d.ResolveTaskID(fullID[:8], "p1")
		if err != nil || got != fullID {
			t.Fatalf("prefix → %q, %v", got, err)
		}
	})

	t.Run("project-scoped", func(t *testing.T) {
		// SYN-3296 in p2 resolves to the p2 row, never the p1 one.
		got, err := d.ResolveTaskID("SYN-3296", "p2")
		if err != nil || got != "99999999-0000-0000-0000-000000000000" {
			t.Fatalf("p2 SYN-3296 → %q, %v", got, err)
		}
	})

	t.Run("unknown key returns ref (not-found downstream)", func(t *testing.T) {
		got, err := d.ResolveTaskID("SYN-9999", "p1")
		if err != nil || got != "SYN-9999" {
			t.Fatalf("unknown key → %q, %v (want the ref back)", got, err)
		}
	})

	t.Run("unknown full-length ref returns ref", func(t *testing.T) {
		const missing = "00000000-0000-0000-0000-000000000000"
		got, err := d.ResolveTaskID(missing, "p1")
		if err != nil || got != missing {
			t.Fatalf("missing full id → %q, %v", got, err)
		}
	})
}

// TestResolveTaskIDAmbiguousPrefix asserts the UUID-prefix path still errors on a
// prefix matching multiple task ids (unchanged behavior).
func TestResolveTaskIDAmbiguousPrefix(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	seedLinearTask(t, d.conn, "abc11111-1111-1111-1111-111111111111", "p1", "one", "2026-01-01T00:00:00Z", "", "")
	seedLinearTask(t, d.conn, "abc22222-2222-2222-2222-222222222222", "p1", "two", "2026-01-01T00:00:00Z", "", "")

	if _, err := d.ResolveTaskID("abc", "p1"); err == nil {
		t.Fatal("expected an ambiguous-prefix error, got nil")
	}
}

// TestResolveLinearTaskIDDeterministic asserts a Linear key matching multiple
// rows in one project resolves deterministically to the oldest-dispatched task.
func TestResolveLinearTaskIDDeterministic(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	defer func() { _ = d.Close() }()

	// Two rows in p1 share SYN-7 (pathological); the oldest dispatched wins.
	seedLinearTask(t, d.conn, "ddddddd1-0000-0000-0000-000000000000", "p1", "newer", "2026-02-01T00:00:00Z", "SYN-7", "")
	seedLinearTask(t, d.conn, "ddddddd0-0000-0000-0000-000000000000", "p1", "older", "2026-01-01T00:00:00Z", "SYN-7", "")

	got, err := d.ResolveTaskID("SYN-7", "p1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "ddddddd0-0000-0000-0000-000000000000" {
		t.Fatalf("expected the oldest-dispatched row, got %q", got)
	}
	// Stable across repeated calls.
	if again, _ := d.ResolveTaskID("SYN-7", "p1"); again != got {
		t.Fatalf("non-deterministic: %q then %q", got, again)
	}
}
