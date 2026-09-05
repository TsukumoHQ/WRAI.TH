package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// The limbo sweep's clock math, relative to a fixed now:
//
//	now         = 2026-06-01
//	blockCut    = now - 7d  = 2026-05-25   (TIER 1 if both clocks are older)
//	archiveCut  = now - 30d = 2026-05-02   (TIER 2 if both clocks are older AND
//	                                         the dispatcher is gone/inactive)
//
// So a timestamp of 2026-05-10 is TIER-1 stale (past 7d, inside 30d); 2026-03-01
// is TIER-2 stale (past 30d); 2026-05-30 is fresh (inside 7d, not limbo).
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

// TIER 1: an in-progress task whose inactive assignee and own activity are both
// past 7d is blocked (naming the dead assignee), reversibly. Dry-run reports the
// disposition but writes nothing; apply performs the CAS block.
func TestLimboSweepTier1BlocksInactiveAssignee(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "boss", "active", "", "", 0)
	seedAgent(t, c, "p1", "dead", "inactive", "", "", 0)
	seedTask(t, c, "t1", "p1", "in-progress", "boss", "dead", "dead", "", "", "", false)
	setTaskActivity(t, c, "t1", limboTS(5, 10))
	setAgentSeen(t, c, "p1", "dead", limboTS(5, 10))

	// dry-run: computed, not written.
	res, err := d.SweepLimboAssignees(limboNow, false)
	if err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	if !res.DryRun || len(res.Blocked) != 1 || len(res.Archived) != 0 {
		t.Fatalf("dry-run: got DryRun=%v blocked=%d archived=%d, want 1 blocked", res.DryRun, len(res.Blocked), len(res.Archived))
	}
	if b := res.Blocked[0]; b.Tier != 1 || b.TaskID != "t1" || !strings.HasPrefix(b.Reason, limboReasonPrefix) {
		t.Fatalf("dry-run disposition wrong: %+v", b)
	}
	if st, _, _ := limboTaskState(t, c, "t1"); st != "in-progress" {
		t.Fatalf("dry-run wrote status: got %q, want in-progress", st)
	}

	// apply: the block lands.
	res, err = d.SweepLimboAssignees(limboNow, true)
	if err != nil {
		t.Fatalf("apply sweep: %v", err)
	}
	if res.DryRun || len(res.Blocked) != 1 {
		t.Fatalf("apply: got DryRun=%v blocked=%d, want 1 blocked, not dry-run", res.DryRun, len(res.Blocked))
	}
	st, reason, _ := limboTaskState(t, c, "t1")
	if st != "blocked" || !strings.HasPrefix(reason, limboReasonPrefix) || !strings.Contains(reason, "dead") {
		t.Fatalf("apply: status=%q reason=%q, want blocked naming 'dead'", st, reason)
	}
}

// The prefilter and Go filters exempt: an ACTIVE assignee, a SERVICE assignee,
// and a dead assignee whose EITHER clock (task activity OR agent last_seen) is
// still recent — both-clock staleness is required, so one fresh clock keeps it.
func TestLimboSweepExemptsActiveServiceAndFreshClocks(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "boss", "active", "", "", 0)
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
	if len(res.Blocked) != 0 || len(res.Archived) != 0 {
		t.Fatalf("nothing should be limbo: blocked=%d archived=%d", len(res.Blocked), len(res.Archived))
	}
	// active + service assignees are filtered out by SQL; only the two dead-but-fresh rows are scanned.
	if res.Scanned != 2 {
		t.Fatalf("scanned=%d, want 2 (dead-but-fresh only; active/service excluded)", res.Scanned)
	}
}

// Re-running the sweep is a no-op on a task it already blocked: the reason prefix
// is the idempotence marker, so no second block / blocked_periods churn — while a
// fresh limbo task in the same pass still gets blocked.
func TestLimboSweepIdempotent(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "boss", "active", "", "", 0)
	seedAgent(t, c, "p1", "dead", "inactive", "", "", 0)

	// already blocked by a prior limbo sweep.
	seedTask(t, c, "t-done", "p1", "blocked", "boss", "dead", "dead", "", "", "", false)
	if _, err := c.Exec(`UPDATE tasks SET blocked_reason = ? WHERE id = 't-done'`,
		limboReasonPrefix+": dead (last_seen "+limboTS(5, 10)+")"); err != nil {
		t.Fatalf("preset blocked_reason: %v", err)
	}
	setTaskActivity(t, c, "t-done", limboTS(5, 10))
	// a fresh candidate still to be caught.
	seedTask(t, c, "t-new", "p1", "in-progress", "boss", "dead", "dead", "", "", "", false)
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

// TIER 2: past 30d on both clocks AND the dispatcher is itself gone → archive
// (reversible, nullable column). If the dispatcher is still alive, the same-age
// task falls to a TIER-1 block instead of an archive.
func TestLimboSweepTier2ArchiveOnlyWhenDispatcherGone(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "boss", "active", "", "", 0)
	seedAgent(t, c, "p1", "dead1", "inactive", "", "", 0)
	seedAgent(t, c, "p1", "dead2", "inactive", "", "", 0)

	// dispatcher "ghostboss" is NOT seeded (gone) → archive-eligible.
	seedTask(t, c, "t-arch", "p1", "in-progress", "ghostboss", "dead1", "dead1", "", "", "", false)
	setTaskActivity(t, c, "t-arch", limboTS(3, 1))
	setAgentSeen(t, c, "p1", "dead1", limboTS(3, 1))
	// dispatcher "boss" is alive → same age blocks (TIER 1), never archives.
	seedTask(t, c, "t-block", "p1", "in-progress", "boss", "dead2", "dead2", "", "", "", false)
	setTaskActivity(t, c, "t-block", limboTS(3, 1))
	setAgentSeen(t, c, "p1", "dead2", limboTS(3, 1))

	res, err := d.SweepLimboAssignees(limboNow, true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Archived) != 1 || res.Archived[0].TaskID != "t-arch" || res.Archived[0].Tier != 2 {
		t.Fatalf("want t-arch archived (tier 2), got archived=%+v", res.Archived)
	}
	if len(res.Blocked) != 1 || res.Blocked[0].TaskID != "t-block" {
		t.Fatalf("want t-block blocked (dispatcher alive), got blocked=%+v", res.Blocked)
	}
	if _, _, archived := limboTaskState(t, c, "t-arch"); !archived {
		t.Fatalf("t-arch archived_at not stamped")
	}
	if st, _, archived := limboTaskState(t, c, "t-block"); st != "blocked" || archived {
		t.Fatalf("t-block: status=%q archived=%v, want blocked + not archived", st, archived)
	}
}

// Dry-run computes both tiers but writes zero rows: a full before/after snapshot
// of (status, archived_at) is identical, while the result still reports the
// dispositions it would have applied.
func TestLimboSweepDryRunWritesNothing(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedAgent(t, c, "p1", "dead", "inactive", "", "", 0)
	// TIER 1 candidate (dispatcher alive).
	seedAgent(t, c, "p1", "boss", "active", "", "", 0)
	seedTask(t, c, "t1", "p1", "in-progress", "boss", "dead", "dead", "", "", "", false)
	setTaskActivity(t, c, "t1", limboTS(5, 10))
	// TIER 2 candidate (dispatcher gone).
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
	if len(res.Blocked)+len(res.Archived) == 0 {
		t.Fatalf("dry-run reported no dispositions; expected it to compute the block+archive")
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
