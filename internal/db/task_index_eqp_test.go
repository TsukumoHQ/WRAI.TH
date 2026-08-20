package db

import (
	"strings"
	"testing"
)

// explainPlan returns the concatenated EXPLAIN QUERY PLAN detail lines for a query.
func explainPlan(t *testing.T, d *DB, q string, args ...any) string {
	t.Helper()
	rows, err := d.ro().Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("EQP: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("EQP scan: %v", err)
		}
		b.WriteString(detail)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EQP rows: %v", err)
	}
	return b.String()
}

// TestSweeperQueriesAreIndexDriven asserts the two periodic tasks sweeper queries
// resolve via their R2 indexes (SEARCH … USING INDEX) rather than a full-table
// SCAN — the EQP evidence the ticket requires.
func TestSweeperQueriesAreIndexDriven(t *testing.T) {
	d := testDB(t)

	// ListStrandedPRTasks (tasks.go) — partial idx_tasks_pr.
	plan := explainPlan(t, d,
		"SELECT id FROM tasks WHERE archived_at IS NULL AND pr_number IS NOT NULL "+
			"AND pr_state IN ('merged','closed') AND status NOT IN ('done','cancelled') "+
			"AND NOT (pr_state = 'closed' AND status = 'blocked') "+
			"ORDER BY last_activity_at DESC LIMIT 500")
	if !strings.Contains(plan, "USING INDEX idx_tasks_pr") {
		t.Errorf("ListStrandedPRTasks not index-driven; plan:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN tasks") && !strings.Contains(plan, "USING INDEX") {
		t.Errorf("ListStrandedPRTasks does a full SCAN; plan:\n%s", plan)
	}

	// GetUnackedTasks (tasks.go) — composite idx_tasks_status_dispatched.
	plan = explainPlan(t, d,
		"SELECT id FROM tasks WHERE status = 'pending' AND archived_at IS NULL AND dispatched_at < '2020-01-01T00:00:00Z'")
	if !strings.Contains(plan, "USING INDEX idx_tasks_status_dispatched") {
		t.Errorf("GetUnackedTasks not index-driven; plan:\n%s", plan)
	}
}
