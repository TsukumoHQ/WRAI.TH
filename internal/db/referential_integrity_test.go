package db

import (
	"database/sql"
	"testing"
)

// seedAgent inserts an agent row with explicit status/is_service so the test can
// craft dead / service / live identities deterministically.
func seedAgent(t *testing.T, conn *sql.DB, project, name, status, profileSlug, reportsTo string, isService int) {
	t.Helper()
	_, err := conn.Exec(
		`INSERT INTO agents (id, name, role, description, registered_at, last_seen, project, reports_to, profile_slug, status, is_executive, interest_tags, max_context_bytes, is_service, cwd)
		 VALUES (?, ?, '', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', ?, ?, ?, ?, 0, '[]', 16384, ?, '')`,
		"ag-"+name, name, project, nullIfEmpty(reportsTo), nullIfEmpty(profileSlug), status, isService,
	)
	if err != nil {
		t.Fatalf("seed agent %s: %v", name, err)
	}
}

// seedTask inserts a task row with the ref columns the test controls; everything
// else gets a safe default. archived=true stamps archived_at (excluded by scans).
func seedTask(t *testing.T, conn *sql.DB, id, project, status, dispatchedBy, assignedTo, claimedBy, profileSlug, parentID, boardID string, archived bool) {
	t.Helper()
	var archivedAt interface{}
	if archived {
		archivedAt = "2026-01-01T00:00:00Z"
	}
	_, err := conn.Exec(
		`INSERT INTO tasks (id, profile_slug, dispatched_by, assigned_to, claimed_by, title, status, project, dispatched_at, parent_task_id, board_id, archived_at, labels, blocked_periods, goal, acceptance_criteria, dod)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '2026-01-01T00:00:00Z', ?, ?, ?, '[]', '[]', '', '[]', '')`,
		id, profileSlug, dispatchedBy, nullIfEmpty(assignedTo), nullIfEmpty(claimedBy), "T "+id, status, project,
		nullIfEmpty(parentID), nullIfEmpty(boardID), archivedAt,
	)
	if err != nil {
		t.Fatalf("seed task %s: %v", id, err)
	}
}

func seedProject(t *testing.T, conn *sql.DB, name string) {
	t.Helper()
	if _, err := conn.Exec(`INSERT OR IGNORE INTO projects (name, planet_type, created_at) VALUES (?, '', '2026-01-01T00:00:00Z')`, name); err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
}

func seedProfile(t *testing.T, conn *sql.DB, project, slug string) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO profiles (id, slug, name, project, created_at, updated_at) VALUES (?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		"pf-"+project+"-"+slug, slug, slug, project,
	); err != nil {
		t.Fatalf("seed profile %s/%s: %v", project, slug, err)
	}
}

func seedMessage(t *testing.T, conn *sql.DB, id, project, from, to string) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO messages (id, from_agent, to_agent, type, subject, content, created_at, project) VALUES (?, ?, ?, 'notification', '', '', '2026-01-01T00:00:00Z', ?)`,
		id, from, to, project,
	); err != nil {
		t.Fatalf("seed message %s: %v", id, err)
	}
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// openCount returns the number of OPEN (unresolved) quarantine rows for a class.
func openCount(t *testing.T, conn *sql.DB, class string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM integrity_quarantine WHERE class = ? AND resolved_at IS NULL`, class).Scan(&n); err != nil {
		t.Fatalf("openCount %s: %v", class, err)
	}
	return n
}

func quarantineRowExists(t *testing.T, conn *sql.DB, class, rowID string) bool {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM integrity_quarantine WHERE class = ? AND row_id = ? AND resolved_at IS NULL`, class, rowID).Scan(&n); err != nil {
		t.Fatalf("quarantineRowExists: %v", err)
	}
	return n > 0
}

// TestReferentialScanDetectsOrphanClasses seeds one representative of every
// orphan class plus the tricky non-orphans (sentinels, a live name==slug
// collision, a service assignee, an archived orphan) and asserts the scan flags
// exactly the right rows.
func TestReferentialScanDetectsOrphanClasses(t *testing.T) {
	d := testDB(t)
	c := d.conn

	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	// 'analytics-lead' is BOTH a registered agent name AND a profile slug — the
	// name==slug collision. A ref to it must RESOLVE (agent row exists), not flag.
	seedProfile(t, c, "p1", "analytics-lead")
	seedAgent(t, c, "p1", "alice", "active", "backend", "", 0)
	seedAgent(t, c, "p1", "analytics-lead", "active", "backend", "", 0)
	seedAgent(t, c, "p1", "zombie", "deleted", "backend", "", 0)       // dead → limbo source
	seedAgent(t, c, "p1", "svc", "inactive", "backend", "", 1)         // service → limbo-exempt
	seedAgent(t, c, "p1", "bob", "active", "backend", "ghost-boss", 0) // orphan_reports_to
	seedAgent(t, c, "p1", "carol", "active", "no-profile", "", 0)      // orphan_agent_profile

	// tasks
	seedTask(t, c, "t-clean", "p1", "in-progress", "alice", "alice", "alice", "backend", "", "", false)        // clean
	seedTask(t, c, "t-odisp", "p1", "pending", "ghost", "", "", "backend", "", "", false)                      // orphan_dispatcher
	seedTask(t, c, "t-oassign", "p1", "pending", "alice", "ghost", "", "backend", "", "", false)               // orphan_assignee
	seedTask(t, c, "t-sentinel", "p1", "pending", "linear", "", "", "", "", "", false)                         // sentinel dispatcher, empty slug → clean
	seedTask(t, c, "t-cron", "p1", "pending", "cron", "", "", "", "", "", false)                               // sentinel → clean
	seedTask(t, c, "t-limbo", "p1", "in-progress", "alice", "zombie", "", "backend", "", "", false)            // limbo (dead assignee)
	seedTask(t, c, "t-svc", "p1", "in-progress", "alice", "svc", "", "backend", "", "", false)                 // service assignee → NOT limbo
	seedTask(t, c, "t-nameslug", "p1", "in-progress", "alice", "analytics-lead", "", "backend", "", "", false) // resolves → clean
	seedTask(t, c, "t-oprofile", "p1", "pending", "alice", "", "", "no-such-profile", "", "", false)           // orphan_profile
	seedTask(t, c, "t-archived", "p1", "cancelled", "ghost", "", "", "backend", "", "", true)                  // archived → excluded
	seedTask(t, c, "t-oproject", "ghost-project", "pending", "linear", "", "", "", "", "", false)              // orphan_task_project (sentinel dispatcher isolates it)
	seedTask(t, c, "t-oboard", "p1", "pending", "linear", "", "", "", "", "no-board", false)                   // orphan_board
	seedTask(t, c, "t-oparent", "p1", "pending", "linear", "", "", "", "no-parent", "", false)                 // orphan_parent

	// messages
	seedMessage(t, c, "m-orecip", "p1", "alice", "ghost")  // orphan_recipient (to)
	seedMessage(t, c, "m-osend", "p1", "ghost", "alice")   // orphan_sender (from)
	seedMessage(t, c, "m-broadcast", "p1", "alice", "*")   // clean (broadcast)
	seedMessage(t, c, "m-team", "p1", "alice", "team:eng") // clean (team addressing)
	seedMessage(t, c, "m-linear", "p1", "linear", "alice") // clean (sentinel sender)

	counts, err := runReferentialScan(c)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	want := map[string]int{
		"orphan_dispatcher":    1, // t-odisp
		"orphan_assignee":      1, // t-oassign
		"limbo":                1, // t-limbo
		"orphan_profile":       1, // t-oprofile
		"orphan_task_project":  1, // t-oproject
		"orphan_board":         1, // t-oboard
		"orphan_parent":        1, // t-oparent
		"orphan_reports_to":    1, // bob
		"orphan_agent_profile": 1, // carol
		"orphan_recipient":     1, // m-orecip
		"orphan_sender":        1, // m-osend
	}
	for class, exp := range want {
		if got := counts[class]; got != exp {
			t.Errorf("class %s: got %d open, want %d", class, got, exp)
		}
	}
	// orphan_claimer must be ZERO (no claimed_by orphan seeded).
	if got := counts["orphan_claimer"]; got != 0 {
		t.Errorf("orphan_claimer: got %d, want 0", got)
	}

	// Explicit non-orphan assertions.
	if quarantineRowExists(t, c, "orphan_dispatcher", "t-sentinel") || quarantineRowExists(t, c, "orphan_dispatcher", "t-cron") {
		t.Error("sentinel dispatcher (linear/cron) must not be flagged")
	}
	if quarantineRowExists(t, c, "orphan_assignee", "t-nameslug") || quarantineRowExists(t, c, "limbo", "t-nameslug") {
		t.Error("live name==slug collision (analytics-lead) must resolve, not flag")
	}
	if quarantineRowExists(t, c, "limbo", "t-svc") {
		t.Error("service-agent assignee must be limbo-exempt")
	}
	if quarantineRowExists(t, c, "orphan_dispatcher", "t-archived") {
		t.Error("archived task must be excluded from the scan")
	}
	// The clean task must be flagged by NOTHING.
	var cleanFlags int
	if err := c.QueryRow(`SELECT COUNT(*) FROM integrity_quarantine WHERE row_id = 't-clean'`).Scan(&cleanFlags); err != nil {
		t.Fatal(err)
	}
	if cleanFlags != 0 {
		t.Errorf("t-clean flagged %d time(s), want 0", cleanFlags)
	}
}

// TestReferentialScanIdempotent proves re-running the scan on an unchanged DB
// adds no duplicate quarantine rows and reports identical counts (the AC's
// "re-run safe on a DB with pre-existing orphans").
func TestReferentialScanIdempotent(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedAgent(t, c, "p1", "alice", "active", "backend", "", 0)
	seedTask(t, c, "t-odisp", "p1", "pending", "ghost", "", "", "backend", "", "", false)
	seedTask(t, c, "t-oassign", "p1", "pending", "alice", "ghost2", "", "backend", "", "", false)

	first, err := runReferentialScan(c)
	if err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	var rowsAfter1 int
	_ = c.QueryRow(`SELECT COUNT(*) FROM integrity_quarantine`).Scan(&rowsAfter1)

	second, err := runReferentialScan(c)
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	var rowsAfter2 int
	_ = c.QueryRow(`SELECT COUNT(*) FROM integrity_quarantine`).Scan(&rowsAfter2)

	if rowsAfter1 != rowsAfter2 {
		t.Errorf("re-run added rows: %d then %d (must be identical — idempotent)", rowsAfter1, rowsAfter2)
	}
	if first["orphan_dispatcher"] != second["orphan_dispatcher"] || first["orphan_assignee"] != second["orphan_assignee"] {
		t.Errorf("re-run changed counts: %v then %v", first, second)
	}
}

// TestReferentialScanResolvesHealedRefs proves a ref that later resolves (the
// missing agent registers) is stamped resolved_at on the next scan — NOT deleted
// (audit trail) — and drops out of the open count.
func TestReferentialScanResolvesHealedRefs(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedTask(t, c, "t-odisp", "p1", "pending", "latecomer", "", "", "backend", "", "", false)

	if _, err := runReferentialScan(c); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if openCount(t, c, "orphan_dispatcher") != 1 {
		t.Fatal("expected 1 open orphan_dispatcher before heal")
	}

	// The referenced agent now exists → the ref resolves.
	seedAgent(t, c, "p1", "latecomer", "active", "backend", "", 0)

	if _, err := runReferentialScan(c); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if openCount(t, c, "orphan_dispatcher") != 0 {
		t.Error("healed ref must drop out of the open count")
	}
	// The row is RESOLVED, not deleted (audit trail preserved).
	var resolved int
	if err := c.QueryRow(`SELECT COUNT(*) FROM integrity_quarantine WHERE class='orphan_dispatcher' AND row_id='t-odisp' AND resolved_at IS NOT NULL`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Errorf("healed quarantine row must be stamped resolved (not deleted): got %d", resolved)
	}
}

// TestReferentialScanReopensRegressedRef proves a ref that resolved and then
// broke again (agent deactivated/deleted, or the row edited) is re-opened rather
// than left stale-resolved.
func TestReferentialScanReopensRegressedRef(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedAgent(t, c, "p1", "flaky", "active", "backend", "", 0)
	seedTask(t, c, "t-odisp", "p1", "pending", "flaky", "", "", "backend", "", "", false)

	// Scan 1: resolves (flaky exists) → no open orphan.
	if _, err := runReferentialScan(c); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if openCount(t, c, "orphan_dispatcher") != 0 {
		t.Fatal("expected 0 open before regression")
	}

	// The agent is hard-removed from the table (simulating a purge) → ref dangles.
	if _, err := c.Exec(`DELETE FROM agents WHERE name = 'flaky' AND project = 'p1'`); err != nil {
		t.Fatal(err)
	}

	if _, err := runReferentialScan(c); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if openCount(t, c, "orphan_dispatcher") != 1 {
		t.Error("regressed ref must be re-opened as an orphan")
	}
}
