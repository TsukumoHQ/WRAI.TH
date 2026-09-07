package db

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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

	counts, err := d.RunReferentialScan()
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

	first, err := d.RunReferentialScan()
	if err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	var rowsAfter1 int
	_ = c.QueryRow(`SELECT COUNT(*) FROM integrity_quarantine`).Scan(&rowsAfter1)

	second, err := d.RunReferentialScan()
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

	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if openCount(t, c, "orphan_dispatcher") != 1 {
		t.Fatal("expected 1 open orphan_dispatcher before heal")
	}

	// The referenced agent now exists → the ref resolves.
	seedAgent(t, c, "p1", "latecomer", "active", "backend", "", 0)

	if _, err := d.RunReferentialScan(); err != nil {
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
	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if openCount(t, c, "orphan_dispatcher") != 0 {
		t.Fatal("expected 0 open before regression")
	}

	// The agent is hard-removed from the table (simulating a purge) → ref dangles.
	if _, err := c.Exec(`DELETE FROM agents WHERE name = 'flaky' AND project = 'p1'`); err != nil {
		t.Fatal(err)
	}

	if _, err := d.RunReferentialScan(); err != nil {
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

// TestOrphanProfilePoolResolver (DEC-wraith-orphan-profile-burndown-1 Q1): a slug
// with NO profiles row is NOT an orphan when any in-project agent carries it (the
// agents pool is the real registry). A slug matched by neither still flags.
func TestOrphanProfilePoolResolver(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	// Agent carries 'wraith-backend' as its profile_slug; no profiles row exists.
	seedAgent(t, c, "p1", "wb-agent", "active", "wraith-backend", "", 0)
	// Task slug resolves via the agents pool → NOT orphan.
	seedTask(t, c, "t-pool", "p1", "pending", "linear", "", "", "wraith-backend", "", "", false)
	// Task slug matches neither profiles nor agents → orphan.
	seedTask(t, c, "t-dead", "p1", "pending", "linear", "", "", "no-such-slug", "", "", false)

	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := openCount(t, c, "orphan_profile"); got != 1 {
		t.Errorf("orphan_profile: got %d open, want 1 (only the fully-dead slug)", got)
	}
	if quarantineRowExists(t, c, "orphan_profile", "t-pool") {
		t.Error("slug carried by an in-project agent must resolve via the pool, not flag")
	}
	if !quarantineRowExists(t, c, "orphan_profile", "t-dead") {
		t.Error("fully-dead slug (no profile, no agent) must still flag")
	}
}

// TestRefChecksDropLowerEquality (B2 AC1): the orphan-resolution clauses use plain
// equality on agents.name / profiles.slug / agents.profile_slug (indexed point
// lookups), so no `LOWER(<ref-col>) = LOWER(...)` remains in refChecks(); the
// defensive `LOWER(x) NOT IN (<sentinels>)` guards are cheap and stay.
func TestRefChecksDropLowerEquality(t *testing.T) {
	// Ban only the agent/profile-SIDE wrappers — those existed solely in the
	// resolution equality, now plain point lookups. The task/message-side
	// `LOWER(t.xxx)`/`LOWER(m.xxx)` legitimately survive inside the kept
	// `... NOT IN (sentinels)` guards, so they are NOT banned here.
	for _, rc := range refChecks() {
		for _, banned := range []string{"LOWER(a.name)", "LOWER(b.name)", "LOWER(p.slug)", "LOWER(a.profile_slug)"} {
			if strings.Contains(rc.orphanSQL, banned) {
				t.Errorf("class %s: %s must be dropped for a plain-equality point lookup:\n%s", rc.class, banned, rc.orphanSQL)
			}
		}
	}
	// The sentinel guard must survive somewhere (it is the reason LOWER stays cheap).
	sentinelKept := false
	for _, rc := range refChecks() {
		if strings.Contains(rc.orphanSQL, ") NOT IN (") {
			sentinelKept = true
			break
		}
	}
	if !sentinelKept {
		t.Error("the LOWER(x) NOT IN (sentinels) guard must be kept")
	}
}

// TestReferentialScanFixturesByteIdentical (B2 AC2): dropping the per-row LOWER()
// wrappers must not change what the scan flags — the fixture DB is lowercase, so
// plain equality resolves exactly what case-insensitive equality did. Asserts the
// pool resolver still resolves an in-project lowercase slug and still flags a
// fully-dead one (the pre-drop contract, unchanged).
func TestReferentialScanFixturesByteIdentical(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "wb-agent", "active", "wraith-backend", "", 0)
	seedTask(t, c, "t-pool", "p1", "pending", "linear", "", "", "wraith-backend", "", "", false)
	seedTask(t, c, "t-dead", "p1", "pending", "linear", "", "", "no-such-slug", "", "", false)

	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if quarantineRowExists(t, c, "orphan_profile", "t-pool") {
		t.Error("lowercase slug carried by an in-project agent must resolve via the pool (plain equality)")
	}
	if !quarantineRowExists(t, c, "orphan_profile", "t-dead") {
		t.Error("fully-dead slug must still flag after the LOWER drop")
	}
	if got := d.checkCaseInvariant(); got != 0 {
		t.Errorf("case-invariant check on lowercase fixtures: got %d violations, want 0", got)
	}
}

// TestCaseInvariantCheckDetectsMixedCase (B2 AC3): checkCaseInvariant reports 0 on
// a lowercase-canonical DB (with cross-project scoping intact) and n=1 when a
// mixed-case row is inserted directly via SQL (bypassing the B1 write-path
// normalization). Revert-check: this asserts on checkCaseInvariant's return, so
// removing the guard breaks the build/test — the mixed-case row cannot go
// unreported.
func TestCaseInvariantCheckDetectsMixedCase(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "wb-agent", "active", "wraith-backend", "", 0)
	seedTask(t, c, "t-ok", "p1", "pending", "linear", "", "", "wraith-backend", "", "", false)
	// Cross-project agent carrying the slug must NOT resolve it (project-scoped).
	seedProject(t, c, "p2")
	seedAgent(t, c, "p2", "other", "active", "cross-slug", "", 0)
	seedTask(t, c, "t-xproj", "p1", "pending", "linear", "", "", "cross-slug", "", "", false)

	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if quarantineRowExists(t, c, "orphan_profile", "t-ok") {
		t.Error("lowercase in-project slug must resolve via the pool")
	}
	if !quarantineRowExists(t, c, "orphan_profile", "t-xproj") {
		t.Error("an agent carrying the slug in another project must not resolve it (project-scoped)")
	}
	if got := d.checkCaseInvariant(); got != 0 {
		t.Errorf("clean lowercase DB: got %d violations, want 0", got)
	}

	// Inject a mixed-case name directly (a write path that skipped normalization).
	if _, err := c.Exec(`UPDATE agents SET name = 'WB-Agent' WHERE id = 'ag-wb-agent'`); err != nil {
		t.Fatalf("inject mixed-case: %v", err)
	}
	if got := d.checkCaseInvariant(); got != 1 {
		t.Errorf("after mixed-case inject: got %d violations, want 1", got)
	}
}

// TestScanLogsCaseInvariantViolationOnce (B2 AC4): a mixed-case stored value makes
// the scan log `integrity: case-invariant violated` exactly once and still
// completes without error — the guard is log-only, it never blocks the scan.
func TestScanLogsCaseInvariantViolationOnce(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "wb-agent", "active", "wraith-backend", "", 0)
	if _, err := c.Exec(`UPDATE agents SET name = 'WB-Agent' WHERE id = 'ag-wb-agent'`); err != nil {
		t.Fatalf("inject mixed-case: %v", err)
	}

	out := captureLog(t, func() {
		if _, err := d.RunReferentialScan(); err != nil {
			t.Fatalf("scan must complete despite a case-invariant violation: %v", err)
		}
	})
	if n := strings.Count(out, "integrity: case-invariant violated"); n != 1 {
		t.Errorf("case-invariant violation log: got %d lines, want exactly 1\n%s", n, out)
	}
}

// TestOrphanProfileTerminalExclusion (DEC-wraith-orphan-profile-burndown-1 Q2):
// done/cancelled tasks are excluded from orphan_profile — a dead slug on a
// finished task is noise, not limbo. The same dead slug on a non-terminal task
// still flags.
func TestOrphanProfileTerminalExclusion(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	// Same fully-dead slug on terminal tasks → NOT flagged.
	seedTask(t, c, "t-done", "p1", "done", "linear", "", "", "dead-slug", "", "", false)
	seedTask(t, c, "t-cancelled", "p1", "cancelled", "linear", "", "", "dead-slug", "", "", false)
	// ...and on a non-terminal task → STILL flagged.
	seedTask(t, c, "t-open", "p1", "in-progress", "linear", "", "", "dead-slug", "", "", false)

	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := openCount(t, c, "orphan_profile"); got != 1 {
		t.Errorf("orphan_profile: got %d open, want 1 (only the non-terminal task)", got)
	}
	if quarantineRowExists(t, c, "orphan_profile", "t-done") || quarantineRowExists(t, c, "orphan_profile", "t-cancelled") {
		t.Error("terminal (done/cancelled) tasks must be excluded from orphan_profile")
	}
	if !quarantineRowExists(t, c, "orphan_profile", "t-open") {
		t.Error("non-terminal fully-dead slug must still flag")
	}
}

// TestOrphanProfileHealPath (AC3): a row flagged under the new definition is
// auto-resolved (resolved_at stamped, row kept) on the next scan once an agent
// joins the pool carrying the slug — the existing heal path, no data rewrite.
func TestOrphanProfileHealPath(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedTask(t, c, "t-heal", "p1", "pending", "linear", "", "", "joins-later", "", "", false)

	// Scan 1: no profile, no agent → flagged.
	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if !quarantineRowExists(t, c, "orphan_profile", "t-heal") {
		t.Fatal("expected orphan_profile flagged before the agent joins the pool")
	}

	// An agent now carries the slug → the condition clears.
	seedAgent(t, c, "p1", "newcomer", "active", "joins-later", "", 0)
	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if openCount(t, c, "orphan_profile") != 0 {
		t.Error("healed orphan_profile must drop out of the open count")
	}
	// Resolved, not deleted (audit trail preserved).
	var resolved int
	if err := c.QueryRow(`SELECT COUNT(*) FROM integrity_quarantine WHERE class='orphan_profile' AND row_id='t-heal' AND resolved_at IS NOT NULL`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Errorf("healed quarantine row must be stamped resolved (not deleted): got %d", resolved)
	}
}

// --- task 1ac2ce6e: writer-starvation split (read phase off the writer) ------

// TestReadPhaseDoesNotHoldWriter (AC1) parks a live scan inside its read phase
// and proves a concurrent writerExec still completes fast — the scan holds no
// writer while it reads. Revert-check: if the read phase ran on d.conn inside a
// tx (the pre-split bug), the writer would be held and the concurrent write would
// block past the 100ms bound, failing this test.
func TestReadPhaseDoesNotHoldWriter(t *testing.T) {
	d := testDB(t)
	seedProject(t, d.conn, "p1")
	seedTask(t, d.conn, "t-odisp", "p1", "pending", "ghost", "", "", "backend", "", "", false)

	readParked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	refScanReadHook = func() {
		once.Do(func() { close(readParked) })
		<-release
	}
	defer func() { refScanReadHook = nil }()

	scanDone := make(chan error, 1)
	go func() { _, err := d.RunReferentialScan(); scanDone <- err }()

	<-readParked // scan is parked inside the read phase (no apply tx open yet)

	writeDone := make(chan error, 1)
	go func() {
		_, err := d.writerExec(
			`INSERT INTO settings (key, value) VALUES ('ac1_probe', '1')
			 ON CONFLICT(key) DO UPDATE SET value = '1'`)
		writeDone <- err
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			close(release)
			t.Fatalf("concurrent writerExec errored during read phase: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(release)
		<-scanDone
		t.Fatal("concurrent writerExec blocked >100ms while the scan read phase was in progress — the scan is holding the writer during reads")
	}

	close(release)
	if err := <-scanDone; err != nil {
		t.Fatalf("scan: %v", err)
	}
}

// TestReadPhaseHoldGuard is the positive control for AC1's 100ms discriminator:
// while a writer tx is genuinely held, a concurrent writerExec MUST block past
// 100ms. This proves the AC1 bound can actually catch a writer being held (so the
// AC1 pass is meaningful, not vacuous).
func TestReadPhaseHoldGuard(t *testing.T) {
	d := testDB(t)
	tx, err := d.beginWriterTx()
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer tx.Rollback()

	writeDone := make(chan error, 1)
	go func() {
		_, err := d.writerExec(
			`INSERT INTO settings (key, value) VALUES ('hold_probe', '1')
			 ON CONFLICT(key) DO UPDATE SET value = '1'`)
		writeDone <- err
	}()

	select {
	case <-writeDone:
		t.Fatal("writerExec completed while a writer tx was held — the 100ms guard would be vacuous")
	case <-time.After(100 * time.Millisecond):
		// expected: the held tx blocks the concurrent write
	}
	_ = tx.Rollback() // release so the parked write can drain
	<-writeDone
}

// TestScanHealBetweenReadAndApply (AC3) proves the accepted one-tick race: a ref
// that heals AFTER the read phase but BEFORE the apply tx is NOT resolved in that
// scan (the read saw it still orphan) and IS resolved by the next scan.
func TestScanHealBetweenReadAndApply(t *testing.T) {
	d := testDB(t)
	seedProject(t, d.conn, "p1")
	seedTask(t, d.conn, "t-odisp", "p1", "pending", "ghost", "", "", "backend", "", "", false)

	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if openCount(t, d.conn, "orphan_dispatcher") != 1 {
		t.Fatalf("expected 1 open orphan_dispatcher after scan 1")
	}

	// Heal the ref (register the missing agent) DURING the second scan, after its
	// read phase has already snapshotted the ref as still-orphan.
	var once sync.Once
	refScanBetweenHook = func() {
		once.Do(func() { seedAgent(t, d.conn, "p1", "ghost", "active", "backend", "", 0) })
	}
	if _, err := d.RunReferentialScan(); err != nil {
		refScanBetweenHook = nil
		t.Fatalf("scan 2: %v", err)
	}
	refScanBetweenHook = nil

	// Scan 2 read the pre-heal state → the row stays OPEN this tick.
	if got := openCount(t, d.conn, "orphan_dispatcher"); got != 1 {
		t.Errorf("scan 2 (heal after read): got %d open, want 1 — the mid-scan heal must NOT resolve this tick", got)
	}

	// Next scan sees the agent → the ref resolves now.
	if _, err := d.RunReferentialScan(); err != nil {
		t.Fatalf("scan 3: %v", err)
	}
	if got := openCount(t, d.conn, "orphan_dispatcher"); got != 0 {
		t.Errorf("scan 3: got %d open, want 0 — the healed ref must resolve on the next scan", got)
	}
}

// recordingExecer captures every statement run on the writer during the apply
// phase while forwarding it to a real tx.
type recordingExecer struct {
	x       execer
	queries []string
}

func (r *recordingExecer) Exec(q string, args ...interface{}) (sql.Result, error) {
	r.queries = append(r.queries, q)
	return r.x.Exec(q, args...)
}

// TestApplyPhaseRunsOnlyRowIDWrites (AC4) proves the apply phase runs ONLY
// INSERT OR IGNORE (explicit values) and UPDATE ... WHERE row_id IN (...) against
// integrity_quarantine — no orphanSQL / SELECT / NOT EXISTS text touches the
// writer — and that it works on a beginWriterTx-derived surface.
func TestApplyPhaseRunsOnlyRowIDWrites(t *testing.T) {
	d := testDB(t)
	now := "2026-01-01T00:00:00Z"
	// Pre-seed one resolved row (reopen target) and one open row (resolve target).
	if _, err := d.conn.Exec(
		`INSERT INTO integrity_quarantine (table_name,row_id,ref_col,ref_value,class,project,detected_at,resolved_at)
		 VALUES ('tasks','r2','dispatched_by','ghost','orphan_dispatcher','p1',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.conn.Exec(
		`INSERT INTO integrity_quarantine (table_name,row_id,ref_col,ref_value,class,project,detected_at)
		 VALUES ('tasks','r3','dispatched_by','ghost','orphan_dispatcher','p1',?)`, now); err != nil {
		t.Fatal(err)
	}

	deltas := []refDelta{{
		c:          refCheck{class: "orphan_dispatcher", table: "tasks", refCol: "dispatched_by"},
		newOrphans: []orphanRow{{rowID: "r1", refValue: "ghost", project: "p1"}},
		reopenIDs:  []string{"r2"},
		resolveIDs: []string{"r3"},
	}}

	tx, err := d.beginWriterTx()
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	rec := &recordingExecer{x: tx.Tx}
	if err := applyRefDeltas(rec, deltas, now); err != nil {
		_ = tx.Rollback()
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if len(rec.queries) == 0 {
		t.Fatal("apply phase ran no writer statements")
	}
	for _, q := range rec.queries {
		up := strings.ToUpper(q)
		if strings.Contains(up, "SELECT") || strings.Contains(up, "NOT EXISTS") {
			t.Errorf("apply statement embeds a read/orphanSQL construct: %q", q)
		}
		insert := strings.Contains(up, "INSERT OR IGNORE INTO INTEGRITY_QUARANTINE")
		update := strings.Contains(up, "UPDATE INTEGRITY_QUARANTINE") && strings.Contains(up, "ROW_ID IN (")
		if !insert && !update {
			t.Errorf("unexpected apply statement shape (not INSERT OR IGNORE / UPDATE row_id IN): %q", q)
		}
	}

	// End state: r1 inserted open, r2 reopened (open), r3 resolved (not open).
	if !quarantineRowExists(t, d.conn, "orphan_dispatcher", "r1") {
		t.Error("r1 should be inserted open")
	}
	if !quarantineRowExists(t, d.conn, "orphan_dispatcher", "r2") {
		t.Error("r2 should be reopened (resolved_at cleared)")
	}
	if quarantineRowExists(t, d.conn, "orphan_dispatcher", "r3") {
		t.Error("r3 should be resolved (no longer open)")
	}
}

// TestLiveScanUsesReaderAndWriterTx (AC4, grep-able) asserts the live scan reads
// on the reader pool and opens its apply via the writerTimeout-bounded
// beginWriterTx, and never references orphanSQL directly in the apply path.
func TestLiveScanUsesReaderAndWriterTx(t *testing.T) {
	src, err := os.ReadFile("referential_integrity.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, "deltas, err := readRefDeltas(rctx, d.ro())") {
		t.Error("RunReferentialScan read phase must run on the reader pool d.ro()")
	}
	if !strings.Contains(text, "tx, err := d.beginWriterTx()") {
		t.Error("RunReferentialScan apply phase must open via beginWriterTx (writerTimeout-bounded)")
	}
	// applyRefDeltas must not embed orphanSQL: its body has no NOT EXISTS / orphanSQL.
	apply := funcBodyText(text, "func applyRefDeltas(")
	if apply == "" {
		t.Fatal("could not isolate applyRefDeltas body")
	}
	if strings.Contains(apply, "orphanSQL") || strings.Contains(apply, "NOT EXISTS") {
		t.Error("applyRefDeltas (writer surface) must not embed orphanSQL text")
	}
}

// funcBodyText returns the source of the function whose signature starts with
// sig, from sig to the matching top-level closing brace.
func funcBodyText(src, sig string) string {
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	depth := 0
	started := false
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				return src[i : j+1]
			}
		}
	}
	return ""
}

// TestBootScanMatchesSplit proves the boot-time single-tx path
// (runReferentialScan on the writer conn) and the live split path
// ((*DB).RunReferentialScan) return identical open counts on the same fixture —
// the two callers share readRefDeltas/applyRefDeltas, so their output cannot drift.
func TestBootScanMatchesSplit(t *testing.T) {
	seed := func(d *DB) {
		seedProject(t, d.conn, "p1")
		seedProfile(t, d.conn, "p1", "backend")
		seedAgent(t, d.conn, "p1", "alice", "active", "backend", "", 0)
		seedTask(t, d.conn, "t1", "p1", "pending", "ghost", "", "", "backend", "", "", false)
		seedTask(t, d.conn, "t2", "p1", "pending", "alice", "ghost2", "", "backend", "", "", false)
		seedTrigger(t, d.conn, "tr-orphan", "ghost-project")
		seedMemory(t, d.conn, "mem-orphan", "ghost-project", false)
	}

	d1 := testDB(t)
	seed(d1)
	bootCounts, err := runReferentialScan(d1.conn)
	if err != nil {
		t.Fatalf("boot scan: %v", err)
	}

	d2 := testDB(t)
	seed(d2)
	splitCounts, err := d2.RunReferentialScan()
	if err != nil {
		t.Fatalf("split scan: %v", err)
	}

	if !reflect.DeepEqual(bootCounts, splitCounts) {
		t.Errorf("boot vs split open counts differ:\n boot=%v\nsplit=%v", bootCounts, splitCounts)
	}
}
