package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// excRow is one exceptions row as the tests read it back.
type excRow struct {
	ID, ClassID, SourceKind, Kind, Code, Retry, Fingerprint, MatchedBy, RaisedBy, Status string
	ResolvedBy, Reason                                                                   *string
}

func excRowsFor(t *testing.T, d *DB, taskID string) []excRow {
	t.Helper()
	rows, err := d.conn.Query(`SELECT id, class_id, source_kind, kind, reason_code, retry_class, fingerprint, matched_by,
		COALESCE(raised_by,''), status, resolved_by, resolution_reason
		FROM exceptions WHERE source_ref = ? ORDER BY opened_at, id`, taskID)
	if err != nil {
		t.Fatalf("query exceptions: %v", err)
	}
	defer rows.Close()
	var out []excRow
	for rows.Next() {
		var r excRow
		if err := rows.Scan(&r.ID, &r.ClassID, &r.SourceKind, &r.Kind, &r.Code, &r.Retry, &r.Fingerprint, &r.MatchedBy,
			&r.RaisedBy, &r.Status, &r.ResolvedBy, &r.Reason); err != nil {
			t.Fatalf("scan exception: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func classOccurrences(t *testing.T, d *DB, classID string) (int, string) {
	t.Helper()
	var n int
	var tpl string
	if err := d.conn.QueryRow(`SELECT occurrences, template FROM exception_classes WHERE id = ?`, classID).Scan(&n, &tpl); err != nil {
		t.Fatalf("read class %s: %v", classID, err)
	}
	return n, tpl
}

// excStartedTask dispatches a task in project p and starts it as agent, so it
// can be blocked (in-progress -> blocked is a valid transition).
func excStartedTask(t *testing.T, d *DB, agent string) string {
	t.Helper()
	task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := d.StartTask(task.ID, agent, "p1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	return task.ID
}

func excBlock(t *testing.T, d *DB, taskID, agent, reason string) {
	t.Helper()
	if _, err := d.BlockTask(taskID, agent, "p1", &reason); err != nil {
		t.Fatalf("block %s: %v", taskID, err)
	}
}

func excRegister(t *testing.T, d *DB, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, _, err := d.RegisterAgent("p1", n, "test", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
}

func excStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestExceptions(t *testing.T) {
	t.Run("BlockOpensOne", func(t *testing.T) {
		d := testDB(t)
		id := excStartedTask(t, d, "dev-a")
		excBlock(t, d, id, "dev-a", "waiting on review of PR #12")
		rows := excRowsFor(t, d, id)
		if len(rows) != 1 {
			t.Fatalf("exceptions for task = %d, want 1", len(rows))
		}
		r := rows[0]
		if r.Status != "open" || r.SourceKind != excSourceTaskBlock || r.RaisedBy != "dev-a" || r.ClassID == "" {
			t.Fatalf("row = %+v, want open task_block raised_by dev-a with a class", r)
		}
		if r.Code != "gate_pending" || r.Kind != "blocker" || r.Retry != "retryable" {
			t.Fatalf("classification = %s/%s/%s, want gate_pending/blocker/retryable", r.Code, r.Kind, r.Retry)
		}
		if n, _ := classOccurrences(t, d, r.ClassID); n != 1 {
			t.Fatalf("class occurrences = %d, want 1", n)
		}
	})

	t.Run("ReblockSameTextSameClass", func(t *testing.T) {
		d := testDB(t)
		excRegister(t, d, "dev-a", "dev-b")
		a := excStartedTask(t, d, "dev-a")
		b := excStartedTask(t, d, "dev-b")
		excBlock(t, d, a, "dev-a", "blocked on dev-a merge of 1f190795 (PR #41) since 2026-09-20T10:00:00Z")
		excBlock(t, d, b, "dev-b", "blocked on dev-b merge of 409fb2e8 (PR #77) since 2026-09-21T11:30:00Z")
		ra, rb := excRowsFor(t, d, a), excRowsFor(t, d, b)
		if len(ra) != 1 || len(rb) != 1 {
			t.Fatalf("rows = %d/%d, want 1/1", len(ra), len(rb))
		}
		if ra[0].ClassID != rb[0].ClassID {
			t.Fatalf("class %s != %s: same text, different id/agent/time must share a class", ra[0].ClassID, rb[0].ClassID)
		}
		if ra[0].Fingerprint != rb[0].Fingerprint || rb[0].MatchedBy != "exact" {
			t.Fatalf("second occurrence fp=%s matched_by=%s, want exact match of %s", rb[0].Fingerprint, rb[0].MatchedBy, ra[0].Fingerprint)
		}
		n, tpl := classOccurrences(t, d, ra[0].ClassID)
		if n != 2 {
			t.Fatalf("class occurrences = %d, want 2", n)
		}
		for _, leak := range []string{"dev-a", "1f190795", "#41", "2026-09-20"} {
			if strings.Contains(tpl, leak) {
				t.Fatalf("template %q still contains %q", tpl, leak)
			}
		}
	})

	t.Run("DrainAttachPersistsVariant", func(t *testing.T) {
		d := testDB(t)
		a := excStartedTask(t, d, "dev-a")
		b := excStartedTask(t, d, "dev-b")
		c := excStartedTask(t, d, "dev-c")
		excBlock(t, d, a, "dev-a", "waiting on the backend health checker cron to ship")
		excBlock(t, d, b, "dev-b", "waiting on the frontend health checker cron to ship")
		excBlock(t, d, c, "dev-c", "waiting on the frontend health checker cron to ship")
		ra, rb, rc := excRowsFor(t, d, a)[0], excRowsFor(t, d, b)[0], excRowsFor(t, d, c)[0]
		if rb.ClassID != ra.ClassID || rb.MatchedBy != "drain" {
			t.Fatalf("variant class=%s matched_by=%s, want drain attach to %s", rb.ClassID, rb.MatchedBy, ra.ClassID)
		}
		if rc.ClassID != ra.ClassID || rc.MatchedBy != "exact" {
			t.Fatalf("repeat of the variant matched_by=%s, want exact (persisted decision)", rc.MatchedBy)
		}
		if _, tpl := classOccurrences(t, d, ra.ClassID); !strings.Contains(tpl, "<*>") {
			t.Fatalf("template %q not generalized", tpl)
		}
	})

	t.Run("UnblockResolvesWithDerivedResolver", func(t *testing.T) {
		d := testDB(t)
		unlimitedWIP(t, d, "p1") // one actor works several tasks in turn
		cases := []struct {
			name, actor, wantBy, wantReason string
			move                            func(id, actor string) error
		}{
			{"blocker-resumes", "dev-a", "self", "fixed", func(id, actor string) error { _, err := d.StartTask(id, actor, "p1"); return err }},
			{"operator-cancels", "human", "human", "cancelled", func(id, actor string) error {
				r := "no longer relevant after the pivot"
				_, err := d.CancelTask(id, actor, "p1", &r)
				return err
			}},
			{"other-agent-resumes", "dev-z", "peer", "fixed", func(id, actor string) error { _, err := d.StartTask(id, actor, "p1"); return err }},
			{"dispatcher-resumes", "cto", "supervisor", "fixed", func(id, actor string) error { _, err := d.StartTask(id, actor, "p1"); return err }},
		}
		for _, tc := range cases {
			id := excStartedTask(t, d, "dev-a")
			excBlock(t, d, id, "dev-a", "blocked by missing fixture")
			if err := tc.move(id, tc.actor); err != nil {
				t.Fatalf("%s: move: %v", tc.name, err)
			}
			rows := excRowsFor(t, d, id)
			if len(rows) != 1 {
				t.Fatalf("%s: rows = %d, want 1 (resolve, no new row)", tc.name, len(rows))
			}
			r := rows[0]
			if r.Status != "resolved" || excStr(r.ResolvedBy) != tc.wantBy {
				t.Fatalf("%s: status=%s resolved_by=%s, want resolved/%s", tc.name, r.Status, excStr(r.ResolvedBy), tc.wantBy)
			}
			if tc.name == "operator-cancels" {
				tc.wantReason = "obsolete" // the cancel reason's lexicon code
			}
			if excStr(r.Reason) != tc.wantReason {
				t.Fatalf("%s: resolution_reason=%s, want %s", tc.name, excStr(r.Reason), tc.wantReason)
			}
		}
	})

	t.Run("CancelWithReason", func(t *testing.T) {
		d := testDB(t)
		task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		r := "superseded by the typed re-dispatch"
		if _, err := d.CancelTask(task.ID, "cto", "p1", &r); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		rows := excRowsFor(t, d, task.ID)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		got := rows[0]
		if got.SourceKind != excSourceTaskCancel || got.Status != "resolved" || got.Kind != "plan_change" || got.Retry != "benign" || got.Code != "superseded" {
			t.Fatalf("classified cancel = %+v, want resolved task_cancel plan_change/benign/superseded", got)
		}
		if excStr(got.ResolvedBy) != "self" || excStr(got.Reason) != "superseded" {
			t.Fatalf("resolved_by=%s reason=%s, want self/superseded", excStr(got.ResolvedBy), excStr(got.Reason))
		}

		prose, err := d.DispatchTask("p1", "dev", "cto", "t2", "", "P2", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		r2 := "Sur-scope corrigé: composants adaptés CIL"
		if _, err := d.CancelTask(prose.ID, "cto", "p1", &r2); err != nil {
			t.Fatalf("cancel prose: %v", err)
		}
		pr := excRowsFor(t, d, prose.ID)
		if len(pr) != 1 || pr[0].Code != excUnclassified || pr[0].Retry != "unknown" {
			t.Fatalf("unclassified cancel = %+v, want one row code=unclassified retry_class=unknown (never benign)", pr)
		}

		bare, err := d.DispatchTask("p1", "dev", "cto", "t3", "", "P2", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if _, err := d.CancelTask(bare.ID, "cto", "p1", nil); err != nil {
			t.Fatalf("cancel bare: %v", err)
		}
		if n := len(excRowsFor(t, d, bare.ID)); n != 0 {
			t.Fatalf("cancel without a reason wrote %d exception(s), want 0", n)
		}
	})

	t.Run("CASLoserWritesNothing", func(t *testing.T) {
		d := testDB(t)
		id := excStartedTask(t, d, "dev-a")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := "waiting on fixture"
				_, errs[i] = d.BlockTask(id, "dev-a", "p1", &r)
			}(i)
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			}
		}
		if ok != 1 {
			t.Fatalf("successful blocks = %d (errs %v), want exactly 1", ok, errs)
		}
		if n := len(excRowsFor(t, d, id)); n != 1 {
			t.Fatalf("exceptions = %d, want 1: the CAS loser must write nothing", n)
		}
	})

	t.Run("TxAtomic", func(t *testing.T) {
		d := testDB(t)
		id := excStartedTask(t, d, "dev-a")
		if _, err := d.conn.Exec(`DROP VIEW exception_occurrences`); err != nil {
			t.Fatalf("drop view: %v", err)
		}
		if _, err := d.conn.Exec(`DROP TABLE exceptions`); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		r := "waiting on fixture"
		if _, err := d.BlockTask(id, "dev-a", "p1", &r); err == nil {
			t.Fatal("BlockTask succeeded with the exceptions table gone, want an error")
		}
		task, err := d.GetTask(id, "p1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if task.Status != "in-progress" || task.BlockedReason != nil {
			t.Fatalf("task status=%s reason=%v after a failed exception write, want in-progress/nil (rolled back)", task.Status, task.BlockedReason)
		}
		var classes int
		_ = d.conn.QueryRow(`SELECT count(*) FROM exception_classes`).Scan(&classes)
		if classes != 0 {
			t.Fatalf("exception_classes = %d after rollback, want 0", classes)
		}
	})

	t.Run("NoModelCallDeterministic", func(t *testing.T) {
		const reason = "approved but merge blocked: main checkout /Users/x/repo is dirty (round 2, 45m)"
		var fps, tpls []string
		for i := 0; i < 2; i++ {
			d := testDB(t)
			excRegister(t, d, "gate-lead")
			id := excStartedTask(t, d, "gate-lead")
			excBlock(t, d, id, "gate-lead", reason)
			r := excRowsFor(t, d, id)[0]
			_, tpl := classOccurrences(t, d, r.ClassID)
			fps, tpls = append(fps, r.Fingerprint), append(tpls, tpl)
		}
		if fps[0] != fps[1] || tpls[0] != tpls[1] {
			t.Fatalf("fresh DBs disagree: fp %v, template %v", fps, tpls)
		}
		if strings.Contains(tpls[0], "/Users/x/repo") || strings.Contains(tpls[0], "45m") {
			t.Fatalf("template %q not parameterized", tpls[0])
		}
	})

	t.Run("TaskColumnsUntouched", func(t *testing.T) {
		d := testDB(t)
		id := excStartedTask(t, d, "dev-a")
		excBlock(t, d, id, "dev-a", "waiting on fixture")
		if _, err := scanTask(d.conn.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)); err != nil {
			t.Fatalf("scanTask over taskColumns: %v (column count and scan targets out of lockstep)", err)
		}
		if strings.Contains(taskColumns, "exception") {
			t.Fatal("taskColumns gained an exception column; the link must stay one-way (exceptions.source_ref)")
		}
	})

	t.Run("OldDBMigrates", func(t *testing.T) {
		d := testDB(t)
		for _, stmt := range []string{`DROP VIEW exception_occurrences`, `DROP TABLE exceptions`, `DROP TABLE exception_classes`} {
			if _, err := d.conn.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		for i := 0; i < 2; i++ {
			if err := migrate(d.conn); err != nil {
				t.Fatalf("migrate #%d: %v", i+1, err)
			}
		}
		for _, name := range []string{"exceptions", "exception_classes", "exception_occurrences"} {
			var n int
			if err := d.conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s present = %d (%v), want 1", name, n, err)
			}
		}
		id := excStartedTask(t, d, "dev-a")
		excBlock(t, d, id, "dev-a", "waiting on fixture")
		if n := len(excRowsFor(t, d, id)); n != 1 {
			t.Fatalf("rows after re-migrate = %d, want 1", n)
		}
	})

	t.Run("QuarantineInView", func(t *testing.T) {
		d := testDB(t)
		if err := d.MarkQuarantine("tasks", "t-1", "assigned_to", "ghost", "orphan_assignee", "p1"); err != nil {
			t.Fatalf("mark: %v", err)
		}
		var kind, status string
		if err := d.conn.QueryRow(`SELECT kind, status FROM exception_occurrences WHERE source_kind = 'quarantine' AND source_ref = 'tasks:t-1'`).Scan(&kind, &status); err != nil {
			t.Fatalf("view row: %v", err)
		}
		if kind != "integrity" || status != "open" {
			t.Fatalf("view row kind=%s status=%s, want integrity/open", kind, status)
		}
	})

	t.Run("LimboBlockSourceDeclared", func(t *testing.T) {
		d := testDB(t)
		id := excStartedTask(t, d, "dev-a")
		ok, err := d.blockLimboTask(id, "p1", "in-progress", "limbo-sweep: dev-a (last_seen 2026-05-10) dispatcher cto", "[]", limboTS(6, 1))
		if err != nil || !ok {
			t.Fatalf("blockLimboTask = %v, %v", ok, err)
		}
		rows := excRowsFor(t, d, id)
		if len(rows) != 1 || rows[0].SourceKind != excSourceLimbo || rows[0].Code != "limbo_sweep" || rows[0].RaisedBy != "relay-sweeper" {
			t.Fatalf("limbo rows = %+v, want one limbo_sweep row raised_by relay-sweeper", rows)
		}
		// Raced no-op: the CAS on the old status finds nothing, writes nothing.
		ok, err = d.blockLimboTask(id, "p1", "in-progress", "limbo-sweep: again", "[]", limboTS(6, 2))
		if err != nil || ok {
			t.Fatalf("raced blockLimboTask = %v, %v, want false, nil", ok, err)
		}
		if n := len(excRowsFor(t, d, id)); n != 1 {
			t.Fatalf("rows after raced limbo = %d, want 1", n)
		}
		// Resuming the task resolves the limbo row.
		if _, err := d.StartTask(id, "dev-a", "p1"); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if r := excRowsFor(t, d, id)[0]; r.Status != "resolved" || excStr(r.ResolvedBy) != "peer" {
			t.Fatalf("limbo row after resume = %+v, want resolved by peer (relay-sweeper raised it)", r)
		}
	})

	t.Run("DeadletterTwoGroupsMax", func(t *testing.T) {
		d := testDB(t)
		excRegister(t, d, "live-a", "live-b", "live-c", "gone-a", "gone-b")
		excMarkGone(t, d, "gone-a", "gone-b")
		m, _, err := d.InsertMessageWithDeliveries("p1", "cto", "*", "notification", "all hands", "body", "{}", "P1", 0, nil, nil,
			[]string{"live-a", "live-b", "live-c", "gone-a", "gone-b"}, "")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		expireMessageNow(t, d, m.ID)
		if _, err := d.ExpireDeliveries(); err != nil {
			t.Fatalf("expire: %v", err)
		}
		got := excDeadletterRows(t, d, m.ID)
		if len(got) != 2 {
			t.Fatalf("want 2 exception rows (live, gone), got %d: %+v", len(got), got)
		}
		for _, r := range got {
			if r.Evidence["live"] != float64(3) || r.Evidence["gone"] != float64(2) || r.Evidence["recipients"] != float64(5) {
				t.Fatalf("evidence = %v, want live=3 gone=2 recipients=5", r.Evidence)
			}
			if r.Kind != "delivery" || r.Status != "resolved" || r.ResolvedBy != "expired" || r.Reason != "expired" {
				t.Fatalf("row = %+v, want delivery resolved expired/expired", r)
			}
		}
		if got[0].Code != "broadcast_unread:gone" || got[1].Code != "broadcast_unread:live" {
			t.Fatalf("codes = %s, %s", got[0].Code, got[1].Code)
		}
		// A re-run of the sweep writes nothing more.
		if _, err := d.ExpireDeliveries(); err != nil {
			t.Fatalf("expire 2: %v", err)
		}
		if n := len(excDeadletterRows(t, d, m.ID)); n != 2 {
			t.Fatalf("re-run wrote more rows: %d", n)
		}
		// A second broadcast with the same shape lands in the same live class.
		m2, _, err := d.InsertMessageWithDeliveries("p1", "cto", "*", "notification", "other text", "body", "{}", "P1", 0, nil, nil,
			[]string{"live-a", "live-b"}, "")
		if err != nil {
			t.Fatalf("insert 2: %v", err)
		}
		expireMessageNow(t, d, m2.ID)
		if _, err := d.ExpireDeliveries(); err != nil {
			t.Fatalf("expire 3: %v", err)
		}
		got2 := excDeadletterRows(t, d, m2.ID)
		if len(got2) != 1 || got2[0].ClassID != got[1].ClassID {
			t.Fatalf("second broadcast: %+v, want 1 row in class %s", got2, got[1].ClassID)
		}
		if n, _ := classOccurrences(t, d, got[1].ClassID); n != 2 {
			t.Fatalf("live class occurrences = %d, want 2", n)
		}
	})

	t.Run("DeadletterRowsUnchanged", func(t *testing.T) {
		run := func(enabled bool) []string {
			d := testDB(t)
			excRegister(t, d, "live-a", "gone-a")
			excMarkGone(t, d, "gone-a")
			for _, s := range []struct{ subj, prio string }{{"one", "P1"}, {"", "P2"}, {"New task: x", "P2"}} {
				m, _, err := d.InsertMessageWithDeliveries("p1", "cto", "*", "notification", s.subj, "body", "{}", s.prio, 0, nil, nil,
					[]string{"live-a", "gone-a"}, "")
				if err != nil {
					t.Fatalf("insert: %v", err)
				}
				expireMessageNow(t, d, m.ID)
			}
			deadletterExceptionsEnabled = enabled
			defer func() { deadletterExceptionsEnabled = true }()
			if _, err := d.ExpireDeliveries(); err != nil {
				t.Fatalf("expire: %v", err)
			}
			var nExc int
			_ = d.conn.QueryRow(`SELECT COUNT(*) FROM exceptions WHERE source_kind = 'deadletter'`).Scan(&nExc)
			if enabled != (nExc > 0) {
				t.Fatalf("enabled=%v but %d deadletter exceptions", enabled, nExc)
			}
			// Every deterministic column; the random id and the sweep clock are
			// checked against the delivery they journal instead.
			rows, err := d.conn.Query(`SELECT dl.to_agent || '|' || dl.from_agent || '|' || dl.priority || '|' || dl.subject || '|' || dl.project
				|| '|' || (dl.created_at = m.created_at) || '|' || (dl.expired_at = dv.expired_at) || '|' || (length(dl.id) = 32)
				FROM deadletter dl JOIN messages m ON m.id = dl.message_id
				JOIN deliveries dv ON dv.message_id = dl.message_id AND dv.to_agent = dl.to_agent
				ORDER BY dl.subject, dl.to_agent`)
			if err != nil {
				t.Fatalf("read deadletter: %v", err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var s string
				_ = rows.Scan(&s)
				out = append(out, s)
			}
			return out
		}
		with, without := run(true), run(false)
		if len(with) != 6 || strings.Join(with, "\n") != strings.Join(without, "\n") {
			t.Fatalf("deadletter differs with the exception write:\nwith:\n%s\nwithout:\n%s", strings.Join(with, "\n"), strings.Join(without, "\n"))
		}
	})

	t.Run("LeaseSweepOpensResolved", func(t *testing.T) {
		d := testDB(t)
		deadID := dispatchClaimed(t, d, "p1", "dead-holder")
		forceLeaseExpired(t, d, deadID, "p1")
		if err := d.DeactivateAgent("p1", "dead-holder"); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		liveID := dispatchClaimed(t, d, "p1", "live-holder")
		forceLeaseExpired(t, d, liveID, "p1")
		swept, err := d.SweepExpiredLeases()
		if err != nil || len(swept) != 1 {
			t.Fatalf("sweep: %v, %d swept", err, len(swept))
		}
		rows := excRowsFor(t, d, deadID)
		if len(rows) != 1 {
			t.Fatalf("dead holder: want 1 exception, got %d", len(rows))
		}
		r := rows[0]
		if r.SourceKind != "lease_sweep" || r.Kind != "lease_expired" || r.Status != "resolved" ||
			excStr(r.ResolvedBy) != "self" || excStr(r.Reason) != "requeued" || r.RaisedBy != "relay-sweeper" {
			t.Fatalf("lease row = %+v", r)
		}
		if n := len(excRowsFor(t, d, liveID)); n != 0 {
			t.Fatalf("live holder: want 0 exceptions, got %d", n)
		}
	})

	t.Run("BackfillCounts", func(t *testing.T) {
		d := testDB(t)
		excBackfillFixture(t, d)
		n := excRerunBackfill(t, d)
		// Checked-in expectation (see excBackfillFixture): 9 task rows in 7
		// classes, 12 deadletter rows in 9 classes, 2 lease rows in 2 classes.
		if n != 23 {
			t.Fatalf("backfill wrote %d rows, want 23", n)
		}
		want := map[string][2]int{ // source_kind -> {rows, classes}
			"task_block": {3, 3}, "limbo_sweep": {2, 1}, "task_cancel": {4, 3},
			"deadletter": {12, 9}, "lease_sweep": {1, 1}, "agent_cascade": {1, 1},
		}
		rows, err := d.conn.Query(`SELECT source_kind, COUNT(*), COUNT(DISTINCT class_id) FROM exceptions GROUP BY source_kind`)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		defer rows.Close()
		got := map[string][2]int{}
		for rows.Next() {
			var k string
			var r, c int
			_ = rows.Scan(&k, &r, &c)
			got[k] = [2]int{r, c}
		}
		for k, w := range want {
			if got[k] != w {
				t.Fatalf("%s: got rows/classes %v, want %v (all: %v)", k, got[k], w, got)
			}
		}
		var classes, open int
		_ = d.conn.QueryRow(`SELECT COUNT(*) FROM exception_classes`).Scan(&classes)
		_ = d.conn.QueryRow(`SELECT COUNT(*) FROM exceptions WHERE status = 'open'`).Scan(&open)
		if classes != 18 || open != 2 {
			t.Fatalf("classes=%d open=%d, want 18 and 2 (the blocked tasks)", classes, open)
		}
		var unknown int
		_ = d.conn.QueryRow(`SELECT COUNT(*) FROM exceptions WHERE source_kind IN ('task_block','task_cancel','limbo_sweep')
			AND status = 'resolved' AND resolved_by != 'unknown'`).Scan(&unknown)
		if unknown != 0 {
			t.Fatalf("%d historical task resolutions invented a resolver", unknown)
		}
	})

	t.Run("BackfillIdempotent", func(t *testing.T) {
		d := testDB(t)
		excBackfillFixture(t, d)
		if n := excRerunBackfill(t, d); n == 0 {
			t.Fatal("first backfill wrote nothing")
		}
		var marker string
		if err := d.conn.QueryRow(`SELECT value FROM settings WHERE key = 'backfill_exceptions_v1'`).Scan(&marker); err != nil || marker != "done" {
			t.Fatalf("marker = %q, %v", marker, err)
		}
		// Second boot: the marker short-circuits.
		if n := backfillExceptionsV1(d.conn); n != 0 {
			t.Fatalf("second boot wrote %d rows", n)
		}
		// Marker lost (a boot that died before setting it): skip-if-exists
		// still writes nothing twice.
		if n := excRerunBackfill(t, d); n != 0 {
			t.Fatalf("re-run without marker wrote %d rows", n)
		}
	})
}

// TestExceptionLexiconUnclassifiedNeverBenign pins the ruling: text the
// lexicon cannot read keeps retry_class=unknown, whatever the default kind.
func TestExceptionLexiconUnclassifiedNeverBenign(t *testing.T) {
	code, kind, retry := classifyReason("Sur-scope corrigé", "plan_change")
	if code != excUnclassified || kind != "plan_change" || retry != "unknown" {
		t.Fatalf("classifyReason = %s/%s/%s, want unclassified/plan_change/unknown", code, kind, retry)
	}
	for _, r := range reasonLexiconV1 {
		if r.code == excUnclassified {
			t.Fatal("lexicon must not contain an explicit unclassified rule")
		}
	}
}

func TestParameterizeHexid(t *testing.T) {
	got := parameterize("task 1f190795 sha c5288ca digits 12345678 word defaced", nil)
	want := "task <hexid> sha <hexid> digits <num> word defaced"
	if got != want {
		t.Fatalf("parameterize = %q, want %q", got, want)
	}
}

// excMarkGone flips registered agents to 'deleted' (a gone recipient/holder).
func excMarkGone(t *testing.T, d *DB, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := d.conn.Exec(`UPDATE agents SET status = 'deleted' WHERE name = ? AND project = 'p1'`, n); err != nil {
			t.Fatalf("mark gone %s: %v", n, err)
		}
	}
}

type excDLRow struct {
	ClassID, Code, Kind, Status, ResolvedBy, Reason string
	Evidence                                        map[string]any
}

// excDeadletterRows reads a message's P7 rows, gone before live.
func excDeadletterRows(t *testing.T, d *DB, msgID string) []excDLRow {
	t.Helper()
	rows, err := d.conn.Query(`SELECT class_id, reason_code, kind, status, COALESCE(resolved_by,''), COALESCE(resolution_reason,''), evidence_json
		FROM exceptions WHERE source_kind = 'deadletter' AND source_ref LIKE ? ORDER BY reason_code`, msgID+":%")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []excDLRow
	for rows.Next() {
		var r excDLRow
		var ev string
		if err := rows.Scan(&r.ClassID, &r.Code, &r.Kind, &r.Status, &r.ResolvedBy, &r.Reason, &ev); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if err := json.Unmarshal([]byte(ev), &r.Evidence); err != nil {
			t.Fatalf("evidence %q: %v", ev, err)
		}
		out = append(out, r)
	}
	return out
}

// excBackfillFixture seeds the backfill fixture: 10 tasks (9 with a reason),
// 20 deadletter rows over 11 messages, 2 lease audit rows.
func excBackfillFixture(t *testing.T, d *DB) {
	t.Helper()
	excRegister(t, d, "live1", "live2", "live3", "live4", "gone1", "gone2")
	excMarkGone(t, d, "gone1", "gone2")
	period := `[{"start":"2026-09-01T00:00:00.000000Z","end":"2026-09-02T00:00:00.000000Z"}]`
	tasks := []struct{ status, reason, periods string }{
		{"blocked", "limbo-sweep: live1 (last_seen 2026-09-01T00:00:00Z) dispatcher cto", period},
		{"blocked", "limbo-sweep: live2 (last_seen 2026-09-02T00:00:00Z) dispatcher cto", period},
		{"cancelled", "superseded by task abc1234", "[]"},
		{"cancelled", "superseded by task def5678", "[]"},
		{"cancelled", "duplicate of 1234abcd", "[]"},
		{"done", "rejected 3 rounds — human needed", period},
		{"cancelled", "obsolete: vague avril", period},
		{"cancelled", "something weird happened", "[]"},
		{"in-progress", "awaiting review from cto", period},
		{"done", "", "[]"},
	}
	for i, tk := range tasks {
		task, err := d.DispatchTask("p1", "dev", "cto", fmt.Sprintf("t%d", i), "", "P2", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if _, err := d.conn.Exec(`UPDATE tasks SET status = ?, blocked_reason = ?, blocked_periods = ?, completed_at = '2026-09-03T00:00:00.000000Z' WHERE id = ?`,
			tk.status, tk.reason, tk.periods, task.ID); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}
	msgs := []struct {
		subject, prio string
		to            []string
	}{
		{"all hands", "P1", []string{"live1", "live2", "live3", "gone1", "gone2"}},
		{"standup", "P1", []string{"live1", "live2", "live3", "live4"}},
		{"New task: foo", "P2", []string{"gone1"}},
		{"New task: bar", "P2", []string{"live1"}},
		{"Cycle niwa: 3/4 done, 0 blocked, 0 in review", "P3", []string{"live1"}},
		{"hello", "P1", []string{"live2"}},
		{"", "P1", []string{"live2"}},
		{"ping", "P2", []string{"gone1"}},
		{"ping again", "P2", []string{"gone2"}},
		{"release", "P2", []string{"live1", "live2", "live3"}},
		{"x", "P1", []string{"live3"}},
	}
	for i, m := range msgs {
		for _, to := range m.to {
			if _, err := d.conn.Exec(`INSERT INTO deadletter (id, message_id, to_agent, from_agent, priority, subject, project, created_at, expired_at)
				VALUES (?, ?, ?, 'cto', ?, ?, 'p1', ?, ?)`, fmt.Sprintf("dl-%d-%s", i, to), fmt.Sprintf("msg-%02d", i), to, m.prio, m.subject,
				fmt.Sprintf("2026-09-0%dT00:00:00.000000Z", 1+i%9), "2026-09-20T00:00:00.000000Z"); err != nil {
				t.Fatalf("seed deadletter: %v", err)
			}
		}
	}
	for i, reason := range []string{"expired-swept", "agent-deactivated"} {
		if _, err := d.conn.Exec(`INSERT INTO audit_log (id, project, actor, action, resource_type, resource_id, summary, reason, created_at)
			VALUES (?, 'p1', 'relay-sweeper', 'lease_transferred', 'task', ?, ?, ?, '2026-09-10T00:00:00.000000Z')`,
			fmt.Sprintf("audit-%d", i), fmt.Sprintf("task-lease-%d", i), "lease gone1 → (released) ("+reason+")", reason); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
}

// excRerunBackfill clears the marker (the fixture is seeded after boot) and
// runs the backfill, returning the rows written.
func excRerunBackfill(t *testing.T, d *DB) int {
	t.Helper()
	if _, err := d.conn.Exec(`DELETE FROM settings WHERE key = 'backfill_exceptions_v1'`); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	return backfillExceptionsV1(d.conn)
}

// failExceptionInserts makes every exceptions INSERT abort (test-only trigger,
// no prod seam) and returns the exception_classes count before the failure, so
// a test can prove no partial class row survives either (c6f6c5e3).
func failExceptionInserts(t *testing.T, d *DB) int {
	t.Helper()
	if _, err := d.conn.Exec(`CREATE TRIGGER exc_fail BEFORE INSERT ON exceptions
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	return countRows(t, d, `SELECT COUNT(*) FROM exception_classes`)
}

// TestExceptionWriteFailure_ExpireDeliveriesStillExpires: the P7 deadletter
// exceptions are an observer; when they fail the deliveries still expire, the
// deadletter journal still lands, and no exception row survives.
func TestExceptionWriteFailure_ExpireDeliveriesStillExpires(t *testing.T) {
	d := testDB(t)
	m, _, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "urgent", "body", "{}", "P0", 0, nil, nil, []string{"bot-a", "bot-b"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	expireMessageNow(t, d, m.ID)
	classes := failExceptionInserts(t, d)

	n, err := d.ExpireDeliveries()
	if err != nil || n != 2 {
		t.Fatalf("expire = %d, %v; want 2, nil", n, err)
	}
	if got := countRows(t, d, `SELECT COUNT(*) FROM deliveries WHERE message_id = ? AND state = 'expired'`, m.ID); got != 2 {
		t.Fatalf("expired deliveries = %d, want 2", got)
	}
	if got := countRows(t, d, `SELECT COUNT(*) FROM deadletter WHERE message_id = ?`, m.ID); got != 2 {
		t.Fatalf("deadletter rows = %d, want 2", got)
	}
	if got := countRows(t, d, `SELECT COUNT(*) FROM exceptions`); got != 0 {
		t.Fatalf("exception rows = %d, want 0", got)
	}
	if got := countRows(t, d, `SELECT COUNT(*) FROM exception_classes`); got != classes {
		t.Fatalf("exception_classes = %d, want %d (no partial write)", got, classes)
	}
}

// TestExceptionWriteFailure_LeaseSweepStillRequeues: a failed P5 exception
// write never keeps a dead holder's lease.
func TestExceptionWriteFailure_LeaseSweepStillRequeues(t *testing.T) {
	d := testDB(t)
	id := dispatchClaimed(t, d, "p1", "dead-holder")
	forceLeaseExpired(t, d, id, "p1")
	if err := d.DeactivateAgent("p1", "dead-holder"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	failExceptionInserts(t, d)

	swept, err := d.SweepExpiredLeases()
	if err != nil || len(swept) != 1 {
		t.Fatalf("sweep: %v, %d swept; want 1", err, len(swept))
	}
	if s, h := taskStatus(t, d, id), leaseHolder(t, d, id); s != "pending" || h != "" {
		t.Fatalf("status=%s holder=%q, want pending/NULL", s, h)
	}
	if n := len(excRowsFor(t, d, id)); n != 0 {
		t.Fatalf("exceptions = %d, want 0", n)
	}
}

// TestExceptionWriteFailure_CascadeStillReleases: a failed P5b exception write
// never keeps a deactivated agent's lease.
func TestExceptionWriteFailure_CascadeStillReleases(t *testing.T) {
	d := testDB(t)
	c := d.conn
	seedProject(t, c, "p1")
	seedProfile(t, c, "p1", "backend")
	seedAgent(t, c, "p1", "holder", "active", "backend", "", 0)
	seedTask(t, c, "t-leased", "p1", "in-progress", "cto", "holder", "holder", "backend", "", "", false)
	setLease(t, d, "t-leased", "holder")
	failExceptionInserts(t, d)

	if err := d.DeactivateAgent("p1", "holder"); err != nil {
		t.Fatal(err)
	}
	res, err := d.CascadeAgentDeactivation("p1", "holder")
	if err != nil || len(res.Released) != 1 {
		t.Fatalf("cascade: %v, released=%d; want 1", err, len(res.Released))
	}
	if s, h := taskStatus(t, d, "t-leased"), leaseHolder(t, d, "t-leased"); s != "pending" || h != "" {
		t.Fatalf("status=%s holder=%q, want pending/NULL", s, h)
	}
	if n := len(excRowsFor(t, d, "t-leased")); n != 0 {
		t.Fatalf("exceptions = %d, want 0", n)
	}
}

// excReasonSources returns source_kind -> reason_source for a task's rows.
func excReasonSources(t *testing.T, d *DB, taskID string) map[string]string {
	t.Helper()
	rows, err := d.conn.Query(`SELECT source_kind, COALESCE(reason_source, '<null>') FROM exceptions WHERE source_ref = ?`, taskID)
	if err != nil {
		t.Fatalf("query reason_source: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, s string
		if err := rows.Scan(&k, &s); err != nil {
			t.Fatalf("scan reason_source: %v", err)
		}
		out[k] = s
	}
	return out
}

func excPendingTask(t *testing.T, d *DB) string {
	t.Helper()
	task, err := d.DispatchTask("p1", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return task.ID
}

// Design dbc317f4 §4: a declared code replaces the lexicon on the block row,
// even when the text reads as another code.
func TestReasonCode_DeclaredOverridesLexiconOnBlock(t *testing.T) {
	d := testDB(t)
	id := excStartedTask(t, d, "dev-a")
	reason := "waiting on review of PR #12" // lexicon: gate_pending
	if _, err := d.BlockTaskWithCode(id, "dev-a", "p1", &reason, "env_blocked"); err != nil {
		t.Fatalf("block: %v", err)
	}
	rows := excRowsFor(t, d, id)
	if len(rows) != 1 || rows[0].SourceKind != excSourceTaskBlock {
		t.Fatalf("rows = %+v, want one task_block", rows)
	}
	if r := rows[0]; r.Code != "env_blocked" || r.Kind != "blocker" || r.Retry != "non_retryable" || r.Status != "open" {
		t.Fatalf("row = %s/%s/%s/%s, want env_blocked/blocker/non_retryable/open", r.Code, r.Kind, r.Retry, r.Status)
	}
	if src := excReasonSources(t, d, id)[excSourceTaskBlock]; src != "declared" {
		t.Fatalf("reason_source = %s, want declared", src)
	}
	task, _ := d.GetTask(id, "p1")
	if excStr(task.BlockedReason) != reason {
		t.Fatalf("blocked_reason = %q, want the free text unchanged", excStr(task.BlockedReason))
	}
}

// A declared cancel of a blocked task resolves the block row with the declared
// code as resolution_reason and records the code on its own task_cancel row.
func TestReasonCode_DeclaredOnCancelFromBlocked(t *testing.T) {
	d := testDB(t)
	id := excStartedTask(t, d, "dev-a")
	excBlock(t, d, id, "dev-a", "waiting on review of PR #12")
	reason := "duplicate of an older ticket" // lexicon: duplicate
	if _, err := d.CancelTaskWithCode(id, "cto", "p1", &reason, "misrouted"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rows := excRowsFor(t, d, id)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the block row + one task_cancel row", len(rows))
	}
	var block, cancel excRow
	for _, r := range rows {
		switch r.SourceKind {
		case excSourceTaskBlock:
			block = r
		case excSourceTaskCancel:
			cancel = r
		}
	}
	if block.Status != "resolved" || excStr(block.Reason) != "misrouted" || block.Code != "gate_pending" {
		t.Fatalf("block row = %+v (reason %s), want resolved as misrouted, its own code kept", block, excStr(block.Reason))
	}
	if cancel.Code != "misrouted" || cancel.Kind != "routing" || cancel.Retry != "non_retryable" || cancel.Status != "resolved" {
		t.Fatalf("cancel row = %+v, want misrouted/routing/non_retryable resolved", cancel)
	}
	src := excReasonSources(t, d, id)
	if src[excSourceTaskCancel] != "declared" || src[excSourceTaskBlock] != "lexicon" {
		t.Fatalf("reason_source = %v, want cancel declared, block lexicon", src)
	}
}

func TestReasonCode_DeclaredOnCancelOfPending(t *testing.T) {
	d := testDB(t)
	id := excPendingTask(t, d)
	reason := "superseded by the v2 plan" // lexicon: superseded
	if _, err := d.CancelTaskWithCode(id, "cto", "p1", &reason, "obsolete"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rows := excRowsFor(t, d, id)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if r := rows[0]; r.SourceKind != excSourceTaskCancel || r.Code != "obsolete" || r.Kind != "plan_change" ||
		r.Retry != "benign" || r.Status != "resolved" || excStr(r.Reason) != "obsolete" {
		t.Fatalf("row = %+v (reason %s), want task_cancel obsolete/plan_change/benign resolved as obsolete", r, excStr(r.Reason))
	}
	if src := excReasonSources(t, d, id)[excSourceTaskCancel]; src != "declared" {
		t.Fatalf("reason_source = %s, want declared", src)
	}
}

// A code with no reason text still records the row; blocked_reason stays NULL.
func TestReasonCode_CodeWithoutTextOnCancel(t *testing.T) {
	d := testDB(t)
	id := excPendingTask(t, d)
	if _, err := d.CancelTaskWithCode(id, "cto", "p1", nil, "postponed"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rows := excRowsFor(t, d, id)
	if len(rows) != 1 || rows[0].Code != "postponed" || rows[0].SourceKind != excSourceTaskCancel {
		t.Fatalf("rows = %+v, want one task_cancel postponed", rows)
	}
	task, _ := d.GetTask(id, "p1")
	if task.BlockedReason != nil {
		t.Fatalf("blocked_reason = %q, want NULL", *task.BlockedReason)
	}
}

// No code: today's lexicon rows, labelled reason_source='lexicon'; a producer
// that passes its own code is labelled 'producer'.
func TestReasonCode_NoCodeKeepsLexicon(t *testing.T) {
	d := testDB(t)
	id := excStartedTask(t, d, "dev-a")
	excBlock(t, d, id, "dev-a", "waiting on review of PR #12")
	if r := excRowsFor(t, d, id)[0]; r.Code != "gate_pending" || r.Kind != "blocker" || r.Retry != "retryable" {
		t.Fatalf("block row = %+v, want the lexicon's gate_pending/blocker/retryable", r)
	}
	pid := excPendingTask(t, d)
	reason := "duplicate of an older ticket"
	if _, err := d.CancelTask(pid, "cto", "p1", &reason); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if r := excRowsFor(t, d, pid)[0]; r.Code != "duplicate" || excStr(r.Reason) != "duplicate" {
		t.Fatalf("cancel row = %+v, want the lexicon's duplicate", r)
	}
	if a, b := excReasonSources(t, d, id)[excSourceTaskBlock], excReasonSources(t, d, pid)[excSourceTaskCancel]; a != "lexicon" || b != "lexicon" {
		t.Fatalf("reason_source = %s / %s, want lexicon / lexicon", a, b)
	}

	tx, err := d.beginWriterTx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	now := "2026-09-27T00:00:00.000000Z"
	if _, err := openExceptionTx(tx, exceptionOpen{
		Project: "p1", SourceKind: excSourceLimbo, SourceRef: "prod-1", Text: "limbo-sweep: x",
		Code: "limbo_sweep", Kind: "limbo", Retry: "retryable", At: now,
	}); err != nil {
		t.Fatalf("producer open: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if src := excReasonSources(t, d, "prod-1")[excSourceLimbo]; src != "producer" {
		t.Fatalf("producer reason_source = %s, want producer", src)
	}
}

// An unknown code is refused before any write: status and exceptions unchanged.
func TestReasonCode_UnknownCodeWritesNothing(t *testing.T) {
	d := testDB(t)
	id := excStartedTask(t, d, "dev-a")
	reason := "x"
	if _, err := d.BlockTaskWithCode(id, "dev-a", "p1", &reason, "not_a_code"); err == nil {
		t.Fatal("block with an unknown code succeeded, want an error")
	}
	if _, err := d.CancelTaskWithCode(id, "dev-a", "p1", &reason, "other"); err == nil {
		t.Fatal("cancel with an unknown code succeeded, want an error")
	}
	task, _ := d.GetTask(id, "p1")
	if task.Status != "in-progress" || task.BlockedReason != nil {
		t.Fatalf("task = %s / %q, want in-progress with no reason", task.Status, excStr(task.BlockedReason))
	}
	if rows := excRowsFor(t, d, id); len(rows) != 0 {
		t.Fatalf("exceptions = %d, want 0", len(rows))
	}
}

// The declared vocabulary is exactly the 10 ruled codes, each with a class.
func TestReasonCode_VocabularyIsClosed(t *testing.T) {
	if len(DeclaredReasonCodes) != 10 || len(declaredReasonClass) != 10 {
		t.Fatalf("vocabulary = %d names / %d classes, want 10 / 10", len(DeclaredReasonCodes), len(declaredReasonClass))
	}
	for _, c := range DeclaredReasonCodes {
		if !IsDeclaredReasonCode(c) {
			t.Fatalf("%s has no kind/retry class", c)
		}
	}
}
