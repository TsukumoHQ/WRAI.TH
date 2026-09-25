package db

import (
	"database/sql"
	"sync"
	"testing"
	"time"
)

// DEC-wraith-obligations-1 slice 1 (task f77efe18): the obligations engine
// tables, instantiation and CAS transitions behind the ACK checker.

func seedAckTask(t *testing.T, d *DB, id, status string, age time.Duration) {
	t.Helper()
	if _, err := d.conn.Exec(`INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, labels, blocked_periods, goal, acceptance_criteria, dod)
		VALUES (?, 'dev', 'cto', 'title', ?, 'p1', ?, '[]', '[]', '', '[]', '')`,
		id, status, time.Now().UTC().Add(-age).Format(memoryTimeFmt)); err != nil {
		t.Fatalf("seed task %s: %v", id, err)
	}
}

func obligationState(t *testing.T, d *DB, normID, taskID string) string {
	t.Helper()
	var s string
	if err := d.conn.QueryRow(`SELECT state FROM obligations WHERE norm_id = ? AND subject_id = ?`, normID, taskID).Scan(&s); err != nil {
		t.Fatalf("state %s/%s: %v", normID, taskID, err)
	}
	return s
}

func activeFor(t *testing.T, d *DB, taskID, normID string) TaskObligation {
	t.Helper()
	obs, err := d.ActiveTaskObligations(time.Now().UTC().Add(-15 * time.Minute))
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	for _, o := range obs {
		if o.TaskID == taskID && o.NormID == normID {
			return o
		}
	}
	t.Fatalf("no active %s obligation for %s in %+v", normID, taskID, obs)
	return TaskObligation{}
}

func instantiate(t *testing.T, d *DB) int {
	t.Helper()
	now := time.Now().UTC()
	n, err := d.InstantiateTaskAck(now.Add(-15*time.Minute), now)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	return n
}

func TestObligations(t *testing.T) {
	t.Run("SchemaAndOldDBMigrates", func(t *testing.T) {
		d := retentionTestDB(t)
		if n := countRows(t, d, `SELECT COUNT(*) FROM norms WHERE id IN ('ack.notify', 'ack.escalate', 'ack.manager', 'ack.human')`); n != 4 {
			t.Fatalf("seeded ACK chain norms = %d, want 4", n)
		}
		if _, err := d.conn.Exec(`DROP TABLE obligations; DROP TABLE norms`); err != nil {
			t.Fatalf("drop: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := migrate(d.conn); err != nil {
				t.Fatalf("migrate %d: %v", i+1, err)
			}
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM norms`); n != 4 {
			t.Fatalf("norms after re-migrate = %d, want 4 (seed idempotent)", n)
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_obligations_%'`); n != 3 {
			t.Fatalf("obligation indexes = %d, want 3", n)
		}
	})

	t.Run("SliceOneDepthBackfilled", func(t *testing.T) {
		d := retentionTestDB(t)
		if _, err := d.conn.Exec(`INSERT INTO obligations (id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state, created_at)
			VALUES ('s1', 'p1', 'ack.escalate', 1, 'h', 'task', 't1', 'assignee_profile', 'dev', 'unfulfilled', '2026-09-25T00:00:00.000000Z')`); err != nil {
			t.Fatalf("seed slice-1 row: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := migrate(d.conn); err != nil {
				t.Fatalf("migrate: %v", err)
			}
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM obligations WHERE id = 's1' AND escalation_depth = 1`); n != 1 {
			t.Fatalf("slice-1 escalate row depth not backfilled to 1")
		}
	})

	t.Run("DedupOnNormBindings", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "t1", "pending", 20*time.Minute)
		if n := instantiate(t, d); n != 4 {
			t.Fatalf("first instantiate opened %d, want 4 (the ACK chain rungs)", n)
		}
		if n := instantiate(t, d); n != 0 {
			t.Fatalf("re-instantiate opened %d, want 0", n)
		}
		if _, err := d.conn.Exec(`INSERT INTO obligations (id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state, created_at)
			SELECT 'dup', project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state, created_at
			FROM obligations WHERE norm_id = 'ack.notify'`); err == nil {
			t.Fatalf("UNIQUE(norm_id, bindings_hash) must refuse a duplicate instance")
		}
	})

	t.Run("NormEnumsClosed", func(t *testing.T) {
		d := retentionTestDB(t)
		// A norm row outside the closed enums (unknown what / while / bearer)
		// is inert: never instantiated, never evaluated.
		for _, q := range []string{
			`INSERT INTO norms (id, subject_kind, trigger, what, while_pred, deadline_kind, bearer_kind, sanction, sanction_template, eval_order)
			 VALUES ('rogue.what', 'task', 'task_pending_unclaimed', 'drop_table', 'task_pending_live', 'time', 'assignee_profile', 'notify_dispatcher_push', 'x %s %d', 0)`,
			`INSERT INTO norms (id, subject_kind, trigger, what, while_pred, deadline_kind, bearer_kind, sanction, sanction_template, eval_order)
			 VALUES ('rogue.while', 'task', 'task_pending_unclaimed', 'task_left_pending', 'always', 'time', 'assignee_profile', 'notify_dispatcher_push', 'x %s %d', 0)`,
			`INSERT INTO norms (id, subject_kind, trigger, what, while_pred, deadline_kind, bearer_kind, sanction, sanction_template, eval_order)
			 VALUES ('rogue.bearer', 'task', 'task_pending_unclaimed', 'task_left_pending', 'task_pending_live', 'time', 'human', 'notify_dispatcher_push', 'x %s %d', 0)`,
		} {
			if _, err := d.conn.Exec(q); err != nil {
				t.Fatalf("seed rogue norm: %v", err)
			}
		}
		seedAckTask(t, d, "t1", "pending", 20*time.Minute)
		if n := instantiate(t, d); n != 4 {
			t.Fatalf("opened %d obligations, want only the 4 ACK chain norms", n)
		}
		if _, err := d.conn.Exec(`INSERT INTO obligations (id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state, created_at)
			VALUES ('r1', 'p1', 'rogue.what', 1, 'h', 'task', 't1', 'assignee_profile', 'dev', 'active', '2026-01-01T00:00:00.000000Z')`); err != nil {
			t.Fatalf("seed rogue obligation: %v", err)
		}
		obs, err := d.ActiveTaskObligations(time.Now().UTC().Add(-15 * time.Minute))
		if err != nil {
			t.Fatalf("active: %v", err)
		}
		for _, o := range obs {
			if _, ack := AckNormDepth[o.NormID]; !ack {
				t.Fatalf("rogue norm %s evaluated", o.NormID)
			}
		}
		if len(obs) != 4 {
			t.Fatalf("active = %d, want the 4 ACK chain obligations", len(obs))
		}
	})

	t.Run("ClockPinsLegacyMarks", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "t1", "pending", 20*time.Minute)
		pinned := time.Date(2026, 9, 25, 12, 0, 0, 123456000, time.UTC)
		d.SetClock(func() time.Time { return pinned })
		if ok, err := d.MarkTaskAckNotified("t1"); err != nil || !ok {
			t.Fatalf("mark ok=%v err=%v", ok, err)
		}
		var got string
		if err := d.conn.QueryRow(`SELECT ack_notified_at FROM tasks WHERE id = 't1'`).Scan(&got); err != nil {
			t.Fatalf("read mark: %v", err)
		}
		if got != "2026-09-25T12:00:00.123456Z" {
			t.Fatalf("mark = %s, want the pinned instant", got)
		}
	})

	t.Run("NotYetEligibleNotInstantiated", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "young", "pending", 5*time.Minute)
		seedAckTask(t, d, "taken", "accepted", 50*time.Minute)
		if n := instantiate(t, d); n != 0 {
			t.Fatalf("opened %d obligations for non-candidates", n)
		}
	})

	t.Run("PreexistingMarksNeverRefire", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "t1", "pending", 50*time.Minute)
		if _, err := d.conn.Exec(`UPDATE tasks SET ack_notified_at = '2026-01-01T00:00:00.000000Z' WHERE id = 't1'`); err != nil {
			t.Fatalf("mark: %v", err)
		}
		instantiate(t, d)
		if s := obligationState(t, d, NormAckNotify, "t1"); s != ObligationUnfulfilled {
			t.Fatalf("notify with a legacy mark opened %s, want unfulfilled", s)
		}
		if s := obligationState(t, d, NormAckEscalate, "t1"); s != ObligationActive {
			t.Fatalf("escalate without a mark opened %s, want active", s)
		}
	})

	t.Run("TransitionCAS", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "t1", "pending", 20*time.Minute)
		instantiate(t, d)
		o := activeFor(t, d, "t1", NormAckNotify)
		now := time.Now().UTC()
		ok, err := d.TransitionTaskAck(o.ID, o.NormID, o.TaskID, now, now)
		if err != nil || !ok {
			t.Fatalf("first transition ok=%v err=%v, want true", ok, err)
		}
		if s := obligationState(t, d, NormAckNotify, "t1"); s != ObligationUnfulfilled {
			t.Fatalf("state %s, want unfulfilled", s)
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM tasks WHERE id = 't1' AND ack_notified_at IS NOT NULL`); n != 1 {
			t.Fatalf("legacy mark not set with the transition")
		}
		if ok, _ := d.TransitionTaskAck(o.ID, o.NormID, o.TaskID, now, now); ok {
			t.Fatalf("second transition of the same obligation must lose the CAS")
		}

		// Legacy guard fails (task claimed) => obligation stays active, no mark.
		seedAckTask(t, d, "t2", "pending", 20*time.Minute)
		instantiate(t, d)
		o2 := activeFor(t, d, "t2", NormAckNotify)
		if _, err := d.conn.Exec(`UPDATE tasks SET status = 'accepted' WHERE id = 't2'`); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if ok, _ := d.TransitionTaskAck(o2.ID, o2.NormID, o2.TaskID, now, now); ok {
			t.Fatalf("transition must fail when the legacy guard refuses")
		}
		if s := obligationState(t, d, NormAckNotify, "t2"); s != ObligationActive {
			t.Fatalf("rolled-back transition left state %s, want active", s)
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM tasks WHERE id = 't2' AND ack_notified_at IS NOT NULL`); n != 0 {
			t.Fatalf("legacy mark set although the tx rolled back")
		}
		if _, err := d.TransitionTaskAck(o2.ID, "ack.bogus", o2.TaskID, now, now); err == nil {
			t.Fatalf("a non-ACK norm must be refused (column whitelist)")
		}
	})

	t.Run("CASRaceOneWinner", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "t1", "pending", 50*time.Minute)
		instantiate(t, d)
		o := activeFor(t, d, "t1", NormAckEscalate)
		now := time.Now().UTC()
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ok, err := d.TransitionTaskAck(o.ID, o.NormID, o.TaskID, now, now); err == nil && ok {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d goroutines won the same transition, want exactly 1", wins)
		}
	})

	t.Run("FulfilledBeatsDeadline", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "t1", "pending", 20*time.Minute)
		instantiate(t, d)
		// Claimed and archived in the same gap: the claim wins.
		if _, err := d.conn.Exec(`UPDATE tasks SET status = 'accepted', archived_at = '2026-01-01T00:00:00.000000Z' WHERE id = 't1'`); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := d.CloseMootTaskObligations(time.Now().UTC()); err != nil {
			t.Fatalf("close: %v", err)
		}
		for _, norm := range []string{NormAckNotify, NormAckEscalate} {
			if s := obligationState(t, d, norm, "t1"); s != ObligationFulfilled {
				t.Fatalf("%s state %s, want fulfilled", norm, s)
			}
		}
	})

	t.Run("WhileFalseGoesInactive", func(t *testing.T) {
		d := retentionTestDB(t)
		for _, id := range []string{"cancelled", "archived", "container", "deleted"} {
			seedAckTask(t, d, id, "pending", 20*time.Minute)
		}
		instantiate(t, d)
		for _, q := range []string{
			`UPDATE tasks SET status = 'cancelled' WHERE id = 'cancelled'`,
			`UPDATE tasks SET archived_at = '2026-01-01T00:00:00.000000Z' WHERE id = 'archived'`,
			`UPDATE tasks SET run_state = 'running' WHERE id = 'container'`,
			`DELETE FROM tasks WHERE id = 'deleted'`,
		} {
			if _, err := d.conn.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		if n, err := d.CloseMootTaskObligations(time.Now().UTC()); err != nil || n != 16 {
			t.Fatalf("closed %d (err %v), want 16 (4 tasks x 4 rungs)", n, err)
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM obligations WHERE state = 'inactive'`); n != 16 {
			t.Fatalf("inactive = %d, want 16", n)
		}
	})

	t.Run("LateDoneStamped", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "t1", "pending", 20*time.Minute)
		instantiate(t, d)
		o := activeFor(t, d, "t1", NormAckNotify)
		now := time.Now().UTC()
		deadline := now.Add(-5 * time.Minute)
		if ok, err := d.TransitionTaskAck(o.ID, o.NormID, o.TaskID, deadline, now); err != nil || !ok {
			t.Fatalf("transition ok=%v err=%v", ok, err)
		}
		if n, _ := d.StampLateDone(now); n != 0 {
			t.Fatalf("stamped %d while the task is still pending", n)
		}
		if _, err := d.conn.Exec(`UPDATE tasks SET status = 'accepted' WHERE id = 't1'`); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if n, err := d.StampLateDone(now); err != nil || n != 1 {
			t.Fatalf("stamped %d (err %v), want 1", n, err)
		}
		var lateBy int64
		if err := d.conn.QueryRow(`SELECT late_by_ms FROM obligations WHERE id = ? AND done_at IS NOT NULL`, o.ID).Scan(&lateBy); err != nil {
			t.Fatalf("late_by_ms: %v", err)
		}
		if lateBy < 299000 || lateBy > 301000 {
			t.Fatalf("late_by_ms = %d, want ~300000", lateBy)
		}
		if n, _ := d.StampLateDone(now); n != 0 {
			t.Fatalf("late-done stamped twice")
		}
	})

	t.Run("NoWriteWhenNothingMoves", func(t *testing.T) {
		d := retentionTestDB(t)
		seedAckTask(t, d, "t1", "pending", 20*time.Minute)
		seedAckTask(t, d, "t2", "pending", 5*time.Minute)
		instantiate(t, d)
		var before, after int
		if err := d.conn.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
			t.Fatalf("total_changes: %v", err)
		}
		now := time.Now().UTC()
		if _, err := d.CloseMootTaskObligations(now); err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, err := d.StampLateDone(now); err != nil {
			t.Fatalf("late-done: %v", err)
		}
		instantiate(t, d)
		if _, err := d.ActiveTaskObligations(now.Add(-15 * time.Minute)); err != nil {
			t.Fatalf("active: %v", err)
		}
		if err := d.conn.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
			t.Fatalf("total_changes: %v", err)
		}
		if after != before {
			t.Fatalf("idle sweep wrote %d rows, want 0", after-before)
		}
	})
}

// pendingClock reads a task's dispatched_at and pending_since verbatim.
func pendingClock(t *testing.T, d *DB, taskID string) (dispatchedAt, pendingSince sql.NullString) {
	t.Helper()
	if err := d.conn.QueryRow(`SELECT dispatched_at, pending_since FROM tasks WHERE id = ?`, taskID).
		Scan(&dispatchedAt, &pendingSince); err != nil {
		t.Fatalf("read clock %s: %v", taskID, err)
	}
	return dispatchedAt, pendingSince
}

// TestAckClockPromoteFromBacklogResets (task c933b2f1 AC1, misfire 27a77033):
// a task dispatched 30 days ago and promoted from backlog starts its ACK chain
// at promote time. One minute after the promote nothing is instantiated (no
// ack.human/ack.manager row); at promote + the rung-0 deadline the chain opens
// and only rung 0 is due.
func TestAckClockPromoteFromBacklogResets(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p1", "dev", "cto", "groomed", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(`UPDATE tasks SET status = 'backlog', dispatched_at = ?, pending_since = ? WHERE id = ?`, old, old, task.ID); err != nil {
		t.Fatalf("backdate into backlog: %v", err)
	}
	if _, changed, err := d.PromoteTask(task.ID, "cto", "p1"); err != nil || !changed {
		t.Fatalf("promote: changed=%v err=%v", changed, err)
	}
	promotedAt := time.Now().UTC()
	if at, since := pendingClock(t, d, task.ID); at.String != old || !since.Valid || since.String == old {
		t.Fatalf("after promote dispatched_at=%q pending_since=%v, want dispatched_at kept and pending_since reset", at.String, since)
	}

	notifyAge := 15 * time.Minute // ack.notify deadline_default_s = 900
	sweep := func(at time.Time) {
		t.Helper()
		if _, err := d.InstantiateTaskAck(at.Add(-notifyAge), at); err != nil {
			t.Fatalf("instantiate at %s: %v", at, err)
		}
	}

	sweep(promotedAt.Add(time.Minute))
	if n := countRows(t, d, `SELECT COUNT(*) FROM obligations WHERE subject_id = ?`, task.ID); n != 0 {
		t.Fatalf("promote+1min opened %d obligation(s), want 0", n)
	}

	at := promotedAt.Add(notifyAge + time.Minute)
	sweep(at)
	views, err := d.MyTaskObligations("p1", "someone", "dev")
	if err != nil {
		t.Fatalf("my obligations: %v", err)
	}
	var due []string
	for _, v := range views {
		if v.SubjectID != task.ID {
			continue
		}
		deadline, err := time.Parse(memoryTimeFmt, v.Deadline)
		if err != nil {
			t.Fatalf("deadline %q of %s: %v", v.Deadline, v.Norm, err)
		}
		if !deadline.After(at) {
			due = append(due, v.Norm)
		}
	}
	if len(due) != 1 || due[0] != NormAckNotify {
		t.Fatalf("due at promote+%s = %v, want only %s", notifyAge+time.Minute, due, NormAckNotify)
	}
}

// TestAckClockDispatchStampsPendingSince (task c933b2f1 AC2): dispatch stamps
// pending_since string-equal to dispatched_at, so the ACK clock of a task that
// never left pending is exactly dispatched_at, as before.
func TestAckClockDispatchStampsPendingSince(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p1", "dev", "cto", "fresh", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	at, since := pendingClock(t, d, task.ID)
	if !since.Valid || since.String != at.String {
		t.Fatalf("pending_since = %v, want %q", since, at.String)
	}

	aged := time.Now().UTC().Add(-20 * time.Minute).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(`UPDATE tasks SET dispatched_at = ?, pending_since = ? WHERE id = ?`, aged, aged, task.ID); err != nil {
		t.Fatalf("age task: %v", err)
	}
	instantiate(t, d)
	if o := activeFor(t, d, task.ID, NormAckNotify); o.DispatchedAt != aged {
		t.Fatalf("ACK clock = %q, want dispatched_at %q", o.DispatchedAt, aged)
	}
}

// TestPendingSinceBackfillEqualsDispatchedAt (task c933b2f1 AC3): a tasks table
// from before the column gets pending_since = dispatched_at on every row, and a
// second migration changes nothing.
func TestPendingSinceBackfillEqualsDispatchedAt(t *testing.T) {
	d := testDB(t)
	if _, err := d.conn.Exec(`ALTER TABLE tasks DROP COLUMN pending_since`); err != nil {
		t.Fatalf("simulate pre-migration table: %v", err)
	}
	seedAckTask(t, d, "a", "pending", 20*time.Minute)
	seedAckTask(t, d, "b", "backlog", 30*24*time.Hour)
	seedAckTask(t, d, "c", "done", time.Hour)

	snapshot := func() string {
		t.Helper()
		rows, err := d.conn.Query(`SELECT id, dispatched_at, COALESCE(pending_since, 'NULL') FROM tasks ORDER BY id`)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		defer rows.Close()
		out := ""
		for rows.Next() {
			var id, at, since string
			if err := rows.Scan(&id, &at, &since); err != nil {
				t.Fatalf("snapshot scan: %v", err)
			}
			if since != at {
				t.Errorf("task %s pending_since = %q, want dispatched_at %q", id, since, at)
			}
			out += id + "|" + at + "|" + since + "\n"
		}
		return out
	}

	if err := migrate(d.conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	first := snapshot()
	if err := migrate(d.conn); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	if second := snapshot(); second != first {
		t.Fatalf("second migration changed rows:\nfirst  %s\nsecond %s", first, second)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM tasks WHERE pending_since IS NULL OR pending_since != dispatched_at`); n != 0 {
		t.Fatalf("%d task(s) with pending_since unset or != dispatched_at", n)
	}
}
