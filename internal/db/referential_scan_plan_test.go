package db

import (
	"fmt"
	"strings"
	"testing"
)

// TestRefScan_TasksClassesUseCoveringIndex pins the query plan of every
// tasks-table class of the referential scan to a COVERING scan of
// idx_tasks_refscan (task 33dab33d). A plain "SCAN t" reads the whole wide tasks
// table per class (~2.9k pages each on the live DB, ~24k per 2-min scan); the
// covering index reads ~90. Checked twice: on a fresh migrated DB (no planner
// stats) and after ANALYZE with a seeded, board-skewed table — the shape where
// the planner otherwise flips orphan_board onto idx_tasks_board + table lookups,
// which PRAGMA optimize can produce in production.
func TestRefScan_TasksClassesUseCoveringIndex(t *testing.T) {
	d := testDB(t)
	c := d.conn

	assertPlans := func(phase string) {
		t.Helper()
		n := 0
		for _, rc := range refChecks() {
			if rc.table != "tasks" {
				continue
			}
			n++
			rows, err := c.Query("EXPLAIN QUERY PLAN " + rc.orphanSQL)
			if err != nil {
				t.Fatalf("%s: explain %s: %v", phase, rc.class, err)
			}
			var plan []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					_ = rows.Close()
					t.Fatalf("%s: scan plan %s: %v", phase, rc.class, err)
				}
				plan = append(plan, detail)
			}
			_ = rows.Close()
			// The tasks access (alias t) must go through the covering index —
			// SCAN, or SEARCH when the planner drives limbo's JOIN from agents —
			// so it never reads the wide table rows. The lookups into
			// agents/profiles/boards/projects are free to use their own indexes.
			var outer string
			for _, p := range plan {
				if strings.HasPrefix(p, "SCAN t ") || p == "SCAN t" || strings.HasPrefix(p, "SEARCH t ") {
					outer = p
					break
				}
			}
			if !strings.Contains(outer, " USING COVERING INDEX idx_tasks_refscan") {
				t.Errorf("%s: class %s outer tasks access = %q, want covering idx_tasks_refscan (full plan %q)",
					phase, rc.class, outer, plan)
			}
		}
		if n != 8 {
			t.Fatalf("%s: expected 8 tasks classes in refChecks(), got %d — update this test with the new class", phase, n)
		}
	}

	assertPlans("fresh")

	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "alice", "active", "backend", "", 0)
	for i := 0; i < 400; i++ {
		board := ""
		if i%50 == 0 {
			board = fmt.Sprintf("b%d", i) // few boarded rows: idx_tasks_board looks selective
		}
		status := "done"
		if i%7 == 0 {
			status = "pending"
		}
		seedTask(t, c, fmt.Sprintf("t%03d", i), "p1", status, "alice", "alice", "", "backend", "", board, i%3 == 0)
	}
	if _, err := c.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	assertPlans("after ANALYZE")
}
