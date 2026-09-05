package db

import (
	"bytes"
	"database/sql"
	"log"
	"strings"
	"testing"
)

// captureLog redirects the stdlib logger (what referential_integrity.go writes
// through — same as the production bridge routes to slog) into a buffer for the
// duration of fn, then restores it. Returns everything logged.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

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

func seedTrigger(t *testing.T, conn *sql.DB, id, project string) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO triggers (id, project, event, profile_slug, cycle, created_at, updated_at) VALUES (?, ?, 'test.event', 'backend', 'once', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		id, project,
	); err != nil {
		t.Fatalf("seed trigger %s: %v", id, err)
	}
}

func seedMemory(t *testing.T, conn *sql.DB, id, project string, archived bool) {
	t.Helper()
	var archivedAt interface{}
	if archived {
		archivedAt = "2026-01-01T00:00:00Z"
	}
	if _, err := conn.Exec(
		`INSERT INTO memories (id, key, value, scope, project, agent_name, created_at, updated_at, archived_at) VALUES (?, ?, 'v', 'project', ?, 'alice', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', ?)`,
		id, id, project, archivedAt,
	); err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
}

func seedWorkflow(t *testing.T, conn *sql.DB, id, project string) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO workflows (id, project, name, created_at, updated_at) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		id, project, id,
	); err != nil {
		t.Fatalf("seed workflow %s: %v", id, err)
	}
}

func seedCycle(t *testing.T, conn *sql.DB, id, project string) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO cycles (id, project, name, created_at, updated_at) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		id, project, id,
	); err != nil {
		t.Fatalf("seed cycle %s: %v", id, err)
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

	// project refs on the 4 tables the Phase 0 scan originally missed
	seedTrigger(t, c, "tr-clean", "p1")
	seedTrigger(t, c, "tr-oproject", "ghost-project")
	seedMemory(t, c, "mem-clean", "p1", false)
	seedMemory(t, c, "mem-oproject", "ghost-project", false)
	seedMemory(t, c, "mem-archived-oproject", "ghost-project", true) // archived → excluded
	seedWorkflow(t, c, "wf-clean", "p1")
	seedWorkflow(t, c, "wf-oproject", "ghost-project")
	seedCycle(t, c, "cy-clean", "p1")
	seedCycle(t, c, "cy-oproject", "ghost-project")

	counts, err := runReferentialScan(c)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	want := map[string]int{
		"orphan_dispatcher":       1, // t-odisp
		"orphan_assignee":         1, // t-oassign
		"limbo":                   1, // t-limbo
		"orphan_profile":          1, // t-oprofile
		"orphan_task_project":     1, // t-oproject
		"orphan_board":            1, // t-oboard
		"orphan_parent":           1, // t-oparent
		"orphan_reports_to":       1, // bob
		"orphan_agent_profile":    1, // carol
		"orphan_recipient":        1, // m-orecip
		"orphan_sender":           1, // m-osend
		"orphan_trigger_project":  1, // tr-oproject
		"orphan_memory_project":   1, // mem-oproject
		"orphan_workflow_project": 1, // wf-oproject
		"orphan_cycle_project":    1, // cy-oproject
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
	if quarantineRowExists(t, c, "orphan_memory_project", "mem-archived-oproject") {
		t.Error("archived memory must be excluded from the scan")
	}
	for _, row := range []struct{ class, id string }{
		{"orphan_trigger_project", "tr-clean"},
		{"orphan_memory_project", "mem-clean"},
		{"orphan_workflow_project", "wf-clean"},
		{"orphan_cycle_project", "cy-clean"},
	} {
		if quarantineRowExists(t, c, row.class, row.id) {
			t.Errorf("%s %s: live project must resolve, not flag", row.class, row.id)
		}
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
	seedTrigger(t, c, "tr-oproject", "ghost-project")
	seedMemory(t, c, "mem-oproject", "ghost-project", false)
	seedWorkflow(t, c, "wf-oproject", "ghost-project")
	seedCycle(t, c, "cy-oproject", "ghost-project")

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
	for _, class := range []string{"orphan_trigger_project", "orphan_memory_project", "orphan_workflow_project", "orphan_cycle_project"} {
		if first[class] != 1 || second[class] != 1 {
			t.Errorf("class %s: got first=%d second=%d, want 1/1", class, first[class], second[class])
		}
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

// TestReferentialScanEmitsDetectLinePerRow: a newly-orphaned row produces one
// `integrity: detect ...` line carrying class, ref=table.col, value and row id.
func TestReferentialScanEmitsDetectLinePerRow(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	// dispatched_by='ghost' with no such agent → orphan_dispatcher; every other
	// ref column empty/valid so this is the only class that fires.
	seedTask(t, c, "t-odisp", "p1", "pending", "ghost", "", "", "", "", "", false)

	out := captureLog(t, func() {
		if _, err := d.RunReferentialScan(); err != nil {
			t.Fatalf("scan: %v", err)
		}
	})
	want := "integrity: detect class=orphan_dispatcher ref=tasks.dispatched_by value=ghost row=t-odisp"
	if !strings.Contains(out, want) {
		t.Fatalf("missing detect line.\nwant substring: %q\ngot:\n%s", want, out)
	}
	if got := strings.Count(out, "row=t-odisp"); got != 1 {
		t.Fatalf("expected exactly one detect line for the row, got %d:\n%s", got, out)
	}
}

// TestReferentialScanEmitsHealLinePerResolution: once the dangling ref resolves
// (the missing agent appears), the next scan emits one `integrity: heal ...`
// line with the row id, an action and resolved_at.
func TestReferentialScanEmitsHealLinePerResolution(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedTask(t, c, "t-odisp", "p1", "pending", "ghost", "", "", "", "", "", false)

	if _, err := d.RunReferentialScan(); err != nil { // opens the quarantine row
		t.Fatalf("first scan: %v", err)
	}
	seedAgent(t, c, "p1", "ghost", "active", "", "", 0) // ref now resolves

	out := captureLog(t, func() {
		if _, err := d.RunReferentialScan(); err != nil {
			t.Fatalf("second scan: %v", err)
		}
	})
	if !strings.Contains(out, "integrity: heal class=orphan_dispatcher row=t-odisp action=ref_resolved resolved_at=") {
		t.Fatalf("missing heal line.\ngot:\n%s", out)
	}
}

// TestReferentialScanPerRowLinesAreTransitionOnly: a re-scan of an unchanged DB
// re-emits NO per-row detect/heal lines. This is the bounded-volume invariant —
// a restart logs only the delta, not every open row.
func TestReferentialScanPerRowLinesAreTransitionOnly(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedTask(t, c, "t-odisp", "p1", "pending", "ghost", "", "", "", "", "", false)

	if _, err := d.RunReferentialScan(); err != nil { // first scan logs the detect
		t.Fatalf("first scan: %v", err)
	}
	out := captureLog(t, func() {
		if _, err := d.RunReferentialScan(); err != nil { // unchanged → silent
			t.Fatalf("second scan: %v", err)
		}
	})
	if strings.Contains(out, "integrity: detect") || strings.Contains(out, "integrity: heal") {
		t.Fatalf("re-scan of unchanged DB must emit no per-row lines, got:\n%s", out)
	}
}

// TestMarkQuarantineEmitsDetectOnlyOnInsert: the on-write soft-mark path logs one
// detect line when it actually inserts, and stays silent on an idempotent dedup.
func TestMarkQuarantineEmitsDetectOnlyOnInsert(t *testing.T) {
	d := testDB(t)

	first := captureLog(t, func() {
		if err := d.MarkQuarantine("tasks", "t-1", "assigned_to", "ghost", "orphan_assignee", "p1"); err != nil {
			t.Fatalf("first mark: %v", err)
		}
	})
	want := "integrity: detect class=orphan_assignee ref=tasks.assigned_to value=ghost row=t-1"
	if !strings.Contains(first, want) {
		t.Fatalf("first MarkQuarantine must log detect.\nwant: %q\ngot:\n%s", want, first)
	}

	second := captureLog(t, func() {
		if err := d.MarkQuarantine("tasks", "t-1", "assigned_to", "ghost", "orphan_assignee", "p1"); err != nil {
			t.Fatalf("second mark: %v", err)
		}
	})
	if strings.Contains(second, "integrity: detect") {
		t.Fatalf("idempotent MarkQuarantine dedup must not re-log detect, got:\n%s", second)
	}
}

// TestLogReferentialCountsKeepsSummaryLine: the aggregate summary line the audit
// relied on is preserved alongside the new per-row lines (regression guard).
func TestLogReferentialCountsKeepsSummaryLine(t *testing.T) {
	out := captureLog(t, func() {
		logReferentialCounts("startup", map[string]int{"orphan_dispatcher": 2, "limbo": 1})
	})
	want := "integrity: startup referential scan — 3 open across 2 class(es): limbo=1 orphan_dispatcher=2"
	if !strings.Contains(out, want) {
		t.Fatalf("summary line changed/dropped.\nwant: %q\ngot:\n%s", want, out)
	}
}
