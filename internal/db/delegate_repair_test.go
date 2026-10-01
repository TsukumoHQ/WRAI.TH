package db

import "testing"

// N3 repair (03958111): non-terminal rows that drifted to the delegating
// service before the fix are re-pointed to their claimer; a done / cancelled row
// is history and stays as is; a row with no non-service claimer is
// left untouched (and logged); a second run changes nothing.
func TestRepairDelegateHeldTasks(t *testing.T) {
	d := testDB(t)
	conn := d.conn
	for _, r := range []struct{ id, assigned, lease, claimed string }{
		{"drift-both", "niwa", "niwa", "dev-a"},
		{"drift-assigned", "niwa", "", "dev-b"},
		{"ambiguous", "niwa", "niwa", ""},
		{"service-claim", "niwa", "", "niwa"},
		{"clean", "dev-c", "dev-c", "dev-c"},
		{"lease-only", "dev-d", "niwa", "dev-e"},
		{"done-row", "niwa", "", "dev-f"}, // terminal: history, left as is
	} {
		status := "in-review"
		if r.id == "done-row" {
			status = "done"
		}
		if _, err := conn.Exec(`INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, assigned_to, lease_holder, claimed_by, labels, blocked_periods)
			VALUES (?, 'dev', 'cto', ?, ?, 'p1', '2026-10-01T00:00:00Z', NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), '[]', '[]')`,
			r.id, r.id, status, r.assigned, r.lease, r.claimed); err != nil {
			t.Fatal(err)
		}
	}
	if n := repairDelegateHeldTasks(conn); n != 3 {
		t.Errorf("repaired %d task(s), want 3", n)
	}
	want := map[string][2]string{
		"drift-both":     {"dev-a", "dev-a"},
		"drift-assigned": {"dev-b", ""},
		"ambiguous":      {"niwa", "niwa"},
		"service-claim":  {"niwa", ""},
		"clean":          {"dev-c", "dev-c"},
		"lease-only":     {"dev-d", "dev-e"}, // a real assignee is never overwritten
		"done-row":       {"niwa", ""},
	}
	for id, w := range want {
		var assigned, lease string
		if err := conn.QueryRow(`SELECT COALESCE(assigned_to, ''), COALESCE(lease_holder, '') FROM tasks WHERE id = ?`, id).Scan(&assigned, &lease); err != nil {
			t.Fatal(err)
		}
		if assigned != w[0] || lease != w[1] {
			t.Errorf("%s: assigned %q lease %q, want %q %q", id, assigned, lease, w[0], w[1])
		}
	}
	if n := repairDelegateHeldTasks(conn); n != 0 {
		t.Errorf("second run repaired %d, want 0 (idempotent)", n)
	}
}
