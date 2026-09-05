package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// The limbo sweep's clock math, relative to a fixed now:
//
//	now       = 2026-06-01
//	blockCut  = now - 7d = 2026-05-25   (BLOCK if BOTH clocks are older AND the
//	                                      dispatcher is itself inactive)
//
// So a timestamp of 2026-05-10 is block-stale (past 7d); 2026-03-01 is deeply
// stale (40d, still just a block — archive was removed); 2026-05-30 is fresh
// (inside 7d, not limbo).
var limboNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func limboTS(month, day int) string {
	return time.Date(2026, time.Month(month), day, 0, 0, 0, 0, time.UTC).Format(memoryTimeFmt)
}

func setTaskActivity(t *testing.T, c *sql.DB, id, ts string) {
	t.Helper()
	if _, err := c.Exec(`UPDATE tasks SET last_activity_at = ? WHERE id = ?`, ts, id); err != nil {
		t.Fatalf("set last_activity_at %s: %v", id, err)
	}
}

func setAgentSeen(t *testing.T, c *sql.DB, project, name, ts string) {
	t.Helper()
	if _, err := c.Exec(`UPDATE agents SET last_seen = ? WHERE name = ? AND project = ?`, ts, name, project); err != nil {
		t.Fatalf("set last_seen %s: %v", name, err)
	}
}

func limboTaskState(t *testing.T, c *sql.DB, id string) (status, reason string, archived bool) {
	t.Helper()
	var r, a sql.NullString
	if err := c.QueryRow(`SELECT status, COALESCE(blocked_reason, ''), archived_at FROM tasks WHERE id = ?`, id).
		Scan(&status, &r, &a); err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return status, r.String, a.Valid && a.String != ""
}

// limboBlockedAuditCount counts the audit rows the sweep writes per block.
func limboBlockedAuditCount(t *testing.T, c *sql.DB) int {
	t.Helper()
	var n int
	if err := c.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'task.limbo_blocked'`).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// BLOCK: an in-progress task whose inactive assignee and own activity are both
// past 7d, AND whose dispatcher is itself inactive, is blocked (naming the dead
// assignee and dispatcher), reversibly. Dry-run reports the disposition but
// writes nothing (no task row, no audit row); apply performs the CAS block and
// records exactly one audit row.
func TestLimboSweepBlocksInactiveAssigneeAndDispatcher(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "deadboss", "inactive", "", "", 0) // dispatcher inactive
	seedAgent(t, c, "p1", "dead", "inactive", "", "", 0)     // assignee inactive
	seedTask(t, c, "t1", "p1", "in-progress", "deadboss", "dead", "dead", "", "", "", false)
	setTaskActivity(t, c, "t1", limboTS(5, 10))
	setAgentSeen(t, c, "p1", "dead", limboTS(5, 10))

	// dry-run: computed, not written (no task row, no audit row).
	res, err := d.SweepLimboAssignees(limboNow, false)
	if err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	if !res.DryRun || len(res.Blocked) != 1 {
		t.Fatalf("dry-run: got DryRun=%v blocked=%d, want 1 blocked", res.DryRun, len(res.Blocked))
	}
	if b := res.Blocked[0]; b.TaskID != "t1" || !strings.HasPrefix(b.Reason, limboReasonPrefix) ||
		!strings.Contains(b.Reason, "dead") || !strings.Contains(b.Reason, "deadboss") {
		t.Fatalf("dry-run disposition wrong: %+v", b)
	}
	if res.Blocked[0].AgeDays != 22 { // 2026-06-01 − 2026-05-10 = 22 days
		t.Fatalf("dry-run AgeDays=%d, want 22", res.Blocked[0].AgeDays)
	}
	if st, _, _ := limboTaskState(t, c, "t1"); st != "in-progress" {
		t.Fatalf("dry-run wrote status: got %q, want in-progress", st)
	}
	if n := limboBlockedAuditCount(t, c); n != 0 {
		t.Fatalf("dry-run wrote %d audit row(s), want 0", n)
	}

	// apply: the block lands, exactly one audit row.
	res, err = d.SweepLimboAssignees(limboNow, true)
	if err != nil {
		t.Fatalf("apply sweep: %v", err)
	}
	if res.DryRun || len(res.Blocked) != 1 {
		t.Fatalf("apply: got DryRun=%v blocked=%d, want 1 blocked, not dry-run", res.DryRun, len(res.Blocked))
	}
	st, reason, archived := limboTaskState(t, c, "t1")
	if st != "blocked" || !strings.HasPrefix(reason, limboReasonPrefix) || !strings.Contains(reason, "dead") {
		t.Fatalf("apply: status=%q reason=%q, want blocked naming 'dead'", st, reason)
	}
	if archived {
		t.Fatalf("apply: task was archived; the sweep must never archive")
	}
	if n := limboBlockedAuditCount(t, c); n != 1 {
		t.Fatalf("apply wrote %d audit row(s), want exactly 1", n)
	}
}

// The prefilter and Go filters exempt: an ACTIVE assignee, a SERVICE assignee,
// and a dead assignee whose EITHER clock (task activity OR agent last_seen) is
// still recent — both-clock staleness is required, so one fresh clock keeps it.
func TestLimboSweepExemptsActiveServiceAndFreshClocks(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "boss", "inactive", "", "", 0) // dispatcher inactive (so only clocks/assignee gate)
	seedAgent(t, c, "p1", "live", "active", "", "", 0)
	seedAgent(t, c, "p1", "svc", "inactive", "", "", 1)   // service → exempt
	seedAgent(t, c, "p1", "deadA", "inactive", "", "", 0) // dead but task fresh
	seedAgent(t, c, "p1", "deadB", "inactive", "", "", 0) // dead but agent seen fresh

	seedTask(t, c, "t-active", "p1", "in-progress", "boss", "live", "live", "", "", "", false)
	seedTask(t, c, "t-svc", "p1", "in-progress", "boss", "svc", "svc", "", "", "", false)
	seedTask(t, c, "t-freshtask", "p1", "in-progress", "boss", "deadA", "deadA", "", "", "", false)
	seedTask(t, c, "t-freshseen", "p1", "in-progress", "boss", "deadB", "deadB", "", "", "", false)

	setTaskActivity(t, c, "t-active", limboTS(5, 10))
	setTaskActivity(t, c, "t-svc", limboTS(5, 10))
	setAgentSeen(t, c, "p1", "svc", limboTS(5, 10))
	// deadA: agent stale, task activity RECENT → keep.
	setTaskActivity(t, c, "t-freshtask", limboTS(5, 30))
	setAgentSeen(t, c, "p1", "deadA", limboTS(5, 10))
	// deadB: task stale, agent last_seen RECENT → keep.
	setTaskActivity(t, c, "t-freshseen", limboTS(5, 10))
	setAgentSeen(t, c, "p1", "deadB", limboTS(5, 30))

	res, err := d.SweepLimboAssignees(limboNow, true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Blocked) != 0 {
		t.Fatalf("nothing should be limbo: blocked=%d", len(res.Blocked))
	}
	// active + service assignees are filtered out by SQL; only the two dead-but-fresh rows are scanned.
	if res.Scanned != 2 {
		t.Fatalf("scanned=%d, want 2 (dead-but-fresh only; active/service excluded)", res.Scanned)
	}
}

// A block requires the DISPATCHER to be inactive too (DEC-wraith-limbo-sweep-rule-1):
// a stale task with a dead assignee is SKIPPED when its dispatcher is active or
// sleeping, and blocked when the dispatcher is gone. A live lead still tracks it.
func TestLimboSweepSkipsWhenDispatcherAlive(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "activeboss", "active", "", "", 0)
	seedAgent(t, c, "p1", "sleepyboss", "sleeping", "", "", 0)
	seedAgent(t, c, "p1", "deadA", "inactive", "", "", 0)
	seedAgent(t, c, "p1", "deadB", "inactive", "", "", 0)
	seedAgent(t, c, "p1", "deadC", "inactive", "", "", 0)

	// same stale assignee clocks for all three; only the dispatcher differs.
	seedTask(t, c, "t-active-disp", "p1", "in-progress", "activeboss", "deadA", "deadA", "", "", "", false)
	seedTask(t, c, "t-sleepy-disp", "p1", "in-progress", "sleepyboss", "deadB", "deadB", "", "", "", false)
	seedTask(t, c, "t-gone-disp", "p1", "in-progress", "ghostboss", "deadC", "deadC", "", "", "", false)
	for _, a := range []string{"deadA", "deadB", "deadC"} {
		setAgentSeen(t, c, "p1", a, limboTS(5, 10))
	}
	for _, id := range []string{"t-active-disp", "t-sleepy-disp", "t-gone-disp"} {
		setTaskActivity(t, c, id, limboTS(5, 10))
	}

	res, err := d.SweepLimboAssignees(limboNow, true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Blocked) != 1 || res.Blocked[0].TaskID != "t-gone-disp" {
		t.Fatalf("want only t-gone-disp blocked (active/sleeping dispatchers skipped), got %+v", res.Blocked)
	}
	if st, _, _ := limboTaskState(t, c, "t-active-disp"); st != "in-progress" {
		t.Fatalf("t-active-disp: status=%q, want kept in-progress", st)
	}
	if st, _, _ := limboTaskState(t, c, "t-sleepy-disp"); st != "in-progress" {
		t.Fatalf("t-sleepy-disp: status=%q, want kept in-progress", st)
	}
}

// Re-running the sweep is a no-op on a task it already blocked: the reason prefix
// is the idempotence marker, so no second block / blocked_periods churn — while a
// fresh limbo task in the same pass still gets blocked.
func TestLimboSweepIdempotent(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "deadboss", "inactive", "", "", 0) // dispatcher inactive
	seedAgent(t, c, "p1", "dead", "inactive", "", "", 0)

	// already blocked by a prior limbo sweep.
	seedTask(t, c, "t-done", "p1", "blocked", "deadboss", "dead", "dead", "", "", "", false)
	if _, err := c.Exec(`UPDATE tasks SET blocked_reason = ? WHERE id = 't-done'`,
		limboReasonPrefix+": dead (last_seen "+limboTS(5, 10)+") dispatcher deadboss"); err != nil {
		t.Fatalf("preset blocked_reason: %v", err)
	}
	setTaskActivity(t, c, "t-done", limboTS(5, 10))
	// a fresh candidate still to be caught.
	seedTask(t, c, "t-new", "p1", "in-progress", "deadboss", "dead", "dead", "", "", "", false)
	setTaskActivity(t, c, "t-new", limboTS(5, 10))
	setAgentSeen(t, c, "p1", "dead", limboTS(5, 10))

	res, err := d.SweepLimboAssignees(limboNow, true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Blocked) != 1 || res.Blocked[0].TaskID != "t-new" {
		t.Fatalf("want exactly t-new blocked, got %+v", res.Blocked)
	}
}

// The sweep NEVER archives (the earlier tier-2 archive was removed by
// DEC-wraith-limbo-sweep-rule-1): a 40d-stale task whose dispatcher is gone ends
// BLOCKED with archived_at still NULL. A same-age task whose dispatcher is alive
// is simply kept.
func TestLimboSweepNeverArchives(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "boss", "active", "", "", 0)
	seedAgent(t, c, "p1", "dead1", "inactive", "", "", 0)
	seedAgent(t, c, "p1", "dead2", "inactive", "", "", 0)

	// dispatcher "ghostboss" is NOT seeded (gone) → blocks (never archives).
	seedTask(t, c, "t-deep", "p1", "in-progress", "ghostboss", "dead1", "dead1", "", "", "", false)
	setTaskActivity(t, c, "t-deep", limboTS(3, 1)) // 40d+ stale
	setAgentSeen(t, c, "p1", "dead1", limboTS(3, 1))
	// dispatcher "boss" is alive → same age is kept (dispatcher tracks it).
	seedTask(t, c, "t-keep", "p1", "in-progress", "boss", "dead2", "dead2", "", "", "", false)
	setTaskActivity(t, c, "t-keep", limboTS(3, 1))
	setAgentSeen(t, c, "p1", "dead2", limboTS(3, 1))

	res, err := d.SweepLimboAssignees(limboNow, true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Blocked) != 1 || res.Blocked[0].TaskID != "t-deep" {
		t.Fatalf("want t-deep blocked, got blocked=%+v", res.Blocked)
	}
	if st, _, archived := limboTaskState(t, c, "t-deep"); st != "blocked" || archived {
		t.Fatalf("t-deep: status=%q archived=%v, want blocked + archived_at NULL", st, archived)
	}
	if st, _, archived := limboTaskState(t, c, "t-keep"); st != "in-progress" || archived {
		t.Fatalf("t-keep: status=%q archived=%v, want kept in-progress + not archived", st, archived)
	}
}

// Dry-run computes the dispositions but writes zero rows — neither task rows nor
// audit rows: a full before/after snapshot of (status, archived_at) is identical
// and the audit_log stays empty, while the result still reports the would-blocks.
func TestLimboSweepDryRunWritesNothing(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "dead", "inactive", "", "", 0)
	// two would-block candidates, both with a gone dispatcher.
	seedTask(t, c, "t1", "p1", "in-progress", "ghostboss", "dead", "dead", "", "", "", false)
	setTaskActivity(t, c, "t1", limboTS(5, 10))
	seedTask(t, c, "t2", "p1", "in-progress", "ghostboss", "dead", "dead", "", "", "", false)
	setTaskActivity(t, c, "t2", limboTS(3, 1))
	setAgentSeen(t, c, "p1", "dead", limboTS(3, 1))

	before := snapshotTasks(t, c)
	res, err := d.SweepLimboAssignees(limboNow, false)
	if err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	after := snapshotTasks(t, c)
	if before != after {
		t.Fatalf("dry-run mutated tasks:\n before=%s\n after =%s", before, after)
	}
	if len(res.Blocked) != 2 {
		t.Fatalf("dry-run reported %d would-blocks; expected 2", len(res.Blocked))
	}
	if n := limboBlockedAuditCount(t, c); n != 0 {
		t.Fatalf("dry-run wrote %d audit row(s), want 0", n)
	}
}

// seedAlreadyBlockedLimbo seeds one already-blocked candidate with a NON-limbo
// reason (both clocks stale, dispatcher gone) plus a fresh limbo task in the same
// pass, and returns the human reason set on the blocked row. Shared by the AC1
// (apply) and AC2 (dry-run) skip tests.
func seedAlreadyBlockedLimbo(t *testing.T, d *DB) (c *sql.DB, humanReason string) {
	t.Helper()
	c = d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "dead", "inactive", "", "", 0) // assignee inactive; dispatcher ghostboss is gone
	humanReason = "blocked by lead: waiting on upstream decision"
	seedTask(t, c, "t-blk", "p1", "blocked", "ghostboss", "dead", "dead", "", "", "", false)
	if _, err := c.Exec(`UPDATE tasks SET blocked_reason = ? WHERE id = 't-blk'`, humanReason); err != nil {
		t.Fatalf("preset blocked_reason: %v", err)
	}
	setTaskActivity(t, c, "t-blk", limboTS(5, 10)) // both clocks past 7d → would-block if not skipped
	setAgentSeen(t, c, "p1", "dead", limboTS(5, 10))
	seedTask(t, c, "t-new", "p1", "in-progress", "ghostboss", "dead", "dead", "", "", "", false)
	setTaskActivity(t, c, "t-new", limboTS(5, 10)) // fresh limbo, proves the skip is per-row
	return c, humanReason
}

// AC1: in apply mode the sweep never touches a task already status='blocked' that
// carries a NON-limbo reason — its original blocked_reason (provenance of WHY it
// was blocked) is preserved, it produces zero audit rows, and it is never in
// res.Blocked. A fresh limbo task in the same pass is still blocked.
func TestLimboSweepSkipsAlreadyBlockedInApply(t *testing.T) {
	d := testDB(t)
	c, humanReason := seedAlreadyBlockedLimbo(t, d)

	res, err := d.SweepLimboAssignees(limboNow, true)
	if err != nil {
		t.Fatalf("apply sweep: %v", err)
	}
	if len(res.Blocked) != 1 || res.Blocked[0].TaskID != "t-new" {
		t.Fatalf("apply: want only t-new blocked (t-blk skipped), got %+v", res.Blocked)
	}
	st, reason, _ := limboTaskState(t, c, "t-blk")
	if st != "blocked" || reason != humanReason {
		t.Fatalf("apply: t-blk status=%q reason=%q, want blocked with reason unchanged (%q)", st, reason, humanReason)
	}
	if n := limboBlockedAuditCount(t, c); n != 1 { // exactly one, for t-new only
		t.Fatalf("apply wrote %d audit row(s), want exactly 1 (t-new only; t-blk skipped)", n)
	}
}

// AC2: in dry-run the already-blocked row emits NO shadow line — it is absent
// from res.Blocked (the relay layer's shadow line is one-per-res.Blocked entry).
func TestLimboSweepSkipsAlreadyBlockedInDryRun(t *testing.T) {
	d := testDB(t)
	c, humanReason := seedAlreadyBlockedLimbo(t, d)

	res, err := d.SweepLimboAssignees(limboNow, false)
	if err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	for _, b := range res.Blocked {
		if b.TaskID == "t-blk" {
			t.Fatalf("dry-run: already-blocked t-blk in res.Blocked (would emit a shadow line): %+v", b)
		}
	}
	if len(res.Blocked) != 1 || res.Blocked[0].TaskID != "t-new" {
		t.Fatalf("dry-run: want only t-new, got %+v", res.Blocked)
	}
	if st, reason, _ := limboTaskState(t, c, "t-blk"); st != "blocked" || reason != humanReason {
		t.Fatalf("dry-run mutated t-blk: status=%q reason=%q", st, reason)
	}
}

// limboAgeDays must report the true whole-day age for BOTH a second-precision
// (no fractional) timestamp and a fractional-second one; unparseable and future
// stamps still yield 0. memoryTimeFmt's zero-padded .000000 layout silently
// mis-parsed no-frac stamps to 0 before the RFC3339 switch.
func TestLimboAgeDaysParsesNoFracAndFrac(t *testing.T) {
	// 2026-07-23 + 44 days = 2026-09-05 (8 to end of July, +31 Aug, +5 Sep).
	nowNoFrac := time.Date(2026, 9, 5, 12, 19, 3, 0, time.UTC)
	if got := limboAgeDays(nowNoFrac, "2026-07-23T12:19:03Z"); got != 44 {
		t.Fatalf("no-frac stamp: AgeDays=%d, want 44", got)
	}
	// fractional-second stamp (memoryTimeFmt): 2026-06-01 − 2026-05-10 = 22 days.
	if got := limboAgeDays(limboNow, limboTS(5, 10)); got != 22 {
		t.Fatalf("frac stamp: AgeDays=%d, want 22", got)
	}
	// unparseable → 0.
	if got := limboAgeDays(nowNoFrac, "not-a-timestamp"); got != 0 {
		t.Fatalf("unparseable stamp: AgeDays=%d, want 0", got)
	}
	// future stamp → 0.
	if got := limboAgeDays(nowNoFrac, "2026-09-06T12:19:03Z"); got != 0 {
		t.Fatalf("future stamp: AgeDays=%d, want 0", got)
	}
}

// snapshotTasks serializes every task's (id,status,archived?) for equality diff.
func snapshotTasks(t *testing.T, c *sql.DB) string {
	t.Helper()
	rows, err := c.Query(`SELECT id, status, archived_at IS NOT NULL FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, status string
		var arch bool
		if err := rows.Scan(&id, &status, &arch); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		b.WriteString(id)
		b.WriteByte('=')
		b.WriteString(status)
		if arch {
			b.WriteString("[archived]")
		}
		b.WriteByte(';')
	}
	return b.String()
}
