package db

import (
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
