package db

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// AC4 of task 98be27bb: existing tasks whose parent_task_id is a short prefix
// are reported; a prefix matching exactly one task in the same project is
// repaired to the full id, anything else is logged and left untouched.
func TestBackfillShortParents_RepairsUniqueAndReportsRest(t *testing.T) {
	d := soakDB(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.conn.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	ins := `INSERT INTO tasks (id, profile_slug, dispatched_by, title, priority, status, project, dispatched_at, parent_task_id)
		VALUES (?, 'dev', 'lead', ?, 'P2', 'pending', ?, '2026-10-03T00:00:00Z', ?)`
	exec(ins, "aaaa1111-0000-0000-0000-000000000001", "epic", "p1", nil)
	exec(ins, "bbbb2222-0000-0000-0000-000000000001", "twin a", "p1", nil)
	exec(ins, "bbbb2222-0000-0000-0000-000000000002", "twin b", "p1", nil)
	exec(ins, "cccc3333-0000-0000-0000-000000000001", "unique child", "p1", "aaaa1111")
	exec(ins, "cccc3333-0000-0000-0000-000000000002", "ambiguous child", "p1", "bbbb2222")
	exec(ins, "cccc3333-0000-0000-0000-000000000003", "orphan child", "p1", "deadbeef")
	exec(ins, "cccc3333-0000-0000-0000-000000000004", "cross-project child", "p2", "aaaa1111")

	refs, err := listShortParentRefs(d.conn)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(refs) != 4 {
		t.Fatalf("report listed %d short parents, want 4: %+v", len(refs), refs)
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	repaired, left, err := backfillShortParents(d.conn)
	log.SetOutput(orig)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if repaired != 1 || left != 3 {
		t.Fatalf("repaired=%d left=%d, want 1 and 3\n%s", repaired, left, buf.String())
	}

	parentOf := func(id string) string {
		var p string
		_ = d.conn.QueryRow(`SELECT COALESCE(parent_task_id, '') FROM tasks WHERE id = ?`, id).Scan(&p)
		return p
	}
	if got := parentOf("cccc3333-0000-0000-0000-000000000001"); got != "aaaa1111-0000-0000-0000-000000000001" {
		t.Errorf("unique prefix not repaired: %q", got)
	}
	for id, want := range map[string]string{
		"cccc3333-0000-0000-0000-000000000002": "bbbb2222",
		"cccc3333-0000-0000-0000-000000000003": "deadbeef",
		"cccc3333-0000-0000-0000-000000000004": "aaaa1111", // parent lives in p1, not p2
	} {
		if got := parentOf(id); got != want {
			t.Errorf("task %s parent = %q, want untouched %q", id, got, want)
		}
	}
	for _, line := range []string{"left as-is: 2 matching", "left as-is: 0 matching", "-> aaaa1111-0000-0000-0000-000000000001"} {
		if !strings.Contains(buf.String(), line) {
			t.Errorf("log missing %q:\n%s", line, buf.String())
		}
	}

	// Idempotent: a second run repairs nothing more and changes nothing.
	repaired, left, err = backfillShortParents(d.conn)
	if err != nil || repaired != 0 || left != 3 {
		t.Fatalf("second run: repaired=%d left=%d err=%v, want 0, 3, nil", repaired, left, err)
	}
}
