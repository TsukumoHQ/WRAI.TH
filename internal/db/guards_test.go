package db

import (
	"errors"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// gxSpec is one instance exception written through the producer path
// (openExceptionTx, so open-point guards evaluate on it).
type gxSpec struct {
	project, code, kind, raisedBy string
	ago                           time.Duration
	resolvedBy, reason            string // non-empty: inserted already resolved
}

func gxOpen(t *testing.T, d *DB, s gxSpec) string {
	t.Helper()
	if s.project == "" {
		s.project = "p"
	}
	if s.code == "" {
		s.code, s.kind = "dead_lane", "routing"
	}
	if s.raisedBy == "" {
		s.raisedBy = "dev-a"
	}
	at := time.Now().Add(-s.ago).UTC().Format(memoryTimeFmt)
	e := exceptionOpen{
		Project: s.project, SourceKind: excSourceTaskBlock, SourceRef: uuid.New().String(), RaisedBy: s.raisedBy,
		Text: "reason " + s.code, Code: s.code, Kind: s.kind, Retry: "non_retryable", At: at,
	}
	if s.resolvedBy != "" {
		e.Resolved = &exceptionResolution{By: s.resolvedBy, Reason: s.reason, At: at}
	}
	tx, err := d.beginWriterTx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := openExceptionTx(tx, e)
	if err != nil {
		t.Fatalf("open exception: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

// gxOrigin is a resolved dead_lane instance raised and resolved (self) by
// dev-a, opened 8 days ago: outside the 7-day routing budget window.
func gxOrigin(t *testing.T, d *DB) string {
	return gxOpen(t, d, gxSpec{ago: 8 * 24 * time.Hour, resolvedBy: "self", reason: "fixed"})
}

// gxSystemic writes a systemic row of code in project directly (the budget
// tick's shape), optionally resolved.
func gxSystemic(t *testing.T, d *DB, project, code, status, reason, owner string, opened time.Time) string {
	t.Helper()
	fp := "sys-" + code
	_, _ = d.conn.Exec(`INSERT OR IGNORE INTO exception_classes (id, kind, reason_code, fingerprint, template, grouping_version,
		matched_by, occurrences, first_seen, last_seen) VALUES (?, 'systemic', ?, ?, ?, 1, 'new', 0, '', '')`,
		"cls-"+fp, code, fp, "systemic: routing/"+code+" over budget")
	id := uuid.New().String()
	at := opened.UTC().Format(memoryTimeFmt)
	var resolvedBy, resolution, resolvedAt any
	if status == "resolved" {
		resolvedBy, resolution, resolvedAt = "peer", reason, at
	}
	if _, err := d.conn.Exec(`INSERT INTO exceptions (id, project, class_id, source_kind, source_ref, kind, reason_code, retry_class,
		fingerprint, matched_by, raised_by, evidence_json, status, resolved_by, resolution_reason, opened_at, resolved_at, owner)
		VALUES (?, ?, ?, 'class_budget', ?, 'systemic', ?, 'non_retryable', ?, 'new', 'relay-sweeper', '{}', ?, ?, ?, ?, ?, ?)`,
		id, project, "cls-"+fp, project+"|"+code+"|"+id, code, fp, status, resolvedBy, resolution, at, resolvedAt, owner); err != nil {
		t.Fatalf("insert systemic: %v", err)
	}
	return id
}

func gxLink(t *testing.T, d *DB, instance, systemic string) {
	t.Helper()
	if _, err := d.conn.Exec(`UPDATE exceptions SET cause_id = ? WHERE id = ?`, systemic, instance); err != nil {
		t.Fatalf("link: %v", err)
	}
}

func gxCompile(d *DB, origin, caller string, mod func(*GuardCompile)) (Guard, error) {
	in := GuardCompile{ExceptionID: origin, Caller: caller, Point: GuardPointOpen, Action: GuardActionSuppress,
		ExpiresIn: 30 * 24 * time.Hour, Now: time.Now()}
	if mod != nil {
		mod(&in)
	}
	return d.CompileResolution(in)
}

func gxMustCompile(t *testing.T, d *DB, origin, caller string, mod func(*GuardCompile)) Guard {
	t.Helper()
	g, err := gxCompile(d, origin, caller, mod)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return g
}

func gxCode(t *testing.T, err error, want string) {
	t.Helper()
	var ge *GuardError
	if !errors.As(err, &ge) || ge.Code != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
}

func gxCount(t *testing.T, d *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.conn.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func gxGuard(t *testing.T, d *DB, id string) Guard {
	t.Helper()
	g, err := d.GetGuard(id)
	if err != nil {
		t.Fatalf("get guard: %v", err)
	}
	return g
}

// gxPromoted compiles a suppress guard from a fresh origin, records 3 old
// shadow hits (outside the budget window) and promotes it by "third".
func gxPromoted(t *testing.T, d *DB, mod func(*GuardCompile)) Guard {
	t.Helper()
	g := gxMustCompile(t, d, gxOrigin(t, d), "dev-a", mod)
	for i := 0; i < 3; i++ {
		gxOpen(t, d, gxSpec{ago: time.Duration(10+i) * 24 * time.Hour, resolvedBy: "self", reason: "fixed"})
	}
	p, err := d.PromoteGuard(g.ID, "third", time.Now())
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	return p
}

func TestGuards(t *testing.T) {
	t.Run("UnknownScopeKeyRejected", func(t *testing.T) {
		d := budgetDB(t)
		o := gxOrigin(t, d)
		_, err := gxCompile(d, o, "dev-a", func(in *GuardCompile) {
			in.Scope = map[string]any{"kind": "routing", "reason_code": "dead_lane", "project": "p", "assignee": "x"}
		})
		gxCode(t, err, GuardErrUnknownScopeKey)
		if n := gxCount(t, d, `SELECT COUNT(*) FROM compiled_guards`); n != 0 {
			t.Fatalf("guards = %d, want 0", n)
		}
	})

	t.Run("AnchorRequired", func(t *testing.T) {
		d := budgetDB(t)
		o := gxOrigin(t, d)
		for _, scope := range []map[string]any{
			{"project": "p", "retry_class": "non_retryable"},
			{"project": "p", "kind": "routing"},
		} {
			_, err := gxCompile(d, o, "dev-a", func(in *GuardCompile) { in.Scope = scope })
			gxCode(t, err, GuardErrAnchorRequired)
		}
		if n := gxCount(t, d, `SELECT COUNT(*) FROM compiled_guards`); n != 0 {
			t.Fatalf("guards = %d, want 0", n)
		}
	})

	t.Run("ScopeMustMatchOrigin", func(t *testing.T) {
		d := budgetDB(t)
		o := gxOrigin(t, d)
		_, err := gxCompile(d, o, "dev-a", func(in *GuardCompile) {
			in.Scope = map[string]any{"kind": "routing", "reason_code": "misrouted", "project": "p"}
		})
		gxCode(t, err, GuardErrScopeExcludes)
		_, err = gxCompile(d, o, "dev-a", func(in *GuardCompile) {
			in.Scope = map[string]any{"kind": "routing", "reason_code": "dead_lane"}
		})
		gxCode(t, err, GuardErrProjectRequired)
		_, err = gxCompile(d, o, "someone-else", nil)
		gxCode(t, err, GuardErrForbidden)
		if n := gxCount(t, d, `SELECT COUNT(*) FROM compiled_guards`); n != 0 {
			t.Fatalf("guards = %d, want 0", n)
		}
	})

	t.Run("ExpiryRequiredAndCapped90d", func(t *testing.T) {
		d := budgetDB(t)
		o := gxOrigin(t, d)
		_, err := gxCompile(d, o, "dev-a", func(in *GuardCompile) { in.ExpiresIn = 0 })
		gxCode(t, err, GuardErrExpiryRequired)
		_, err = gxCompile(d, o, "dev-a", func(in *GuardCompile) { in.ExpiresIn = 91 * 24 * time.Hour })
		gxCode(t, err, GuardErrExpiryTooLong)
		if n := gxCount(t, d, `SELECT COUNT(*) FROM compiled_guards`); n != 0 {
			t.Fatalf("guards = %d, want 0", n)
		}
		g := gxMustCompile(t, d, o, "dev-a", func(in *GuardCompile) { in.ExpiresIn = 90 * 24 * time.Hour })
		if g.Mode != GuardModeShadow || g.ExpiresAt == "" {
			t.Fatalf("guard mode=%s expires=%q, want shadow with expiry", g.Mode, g.ExpiresAt)
		}
	})

	t.Run("ReplayClassifiesAgreeConflict", func(t *testing.T) {
		d := budgetDB(t)
		now := time.Now()
		day := 24 * time.Hour
		// 12 dead_lane instances in p over 100 days; 3 older than 90 days.
		var inst []string
		for i := 0; i < 12; i++ {
			ago := time.Duration(5+i*8) * day // 5d .. 93d: 12 rows, 11th and 12th > 90d
			if i >= 9 {
				ago = time.Duration(91+i) * day
			}
			inst = append(inst, gxOpen(t, d, gxSpec{ago: ago, resolvedBy: "self", reason: "fixed"}))
		}
		fixed := gxSystemic(t, d, "p", "dead_lane", "resolved", "fixed", "lead", now.Add(-50*day))
		wontFix := gxSystemic(t, d, "p", "dead_lane", "resolved", "wont_fix", "lead", now.Add(-40*day))
		open := gxSystemic(t, d, "p", "dead_lane", "open", "", "lead", now.Add(-2*day))
		gxLink(t, d, inst[0], fixed)
		gxLink(t, d, inst[1], fixed)
		gxLink(t, d, inst[2], wontFix)
		gxLink(t, d, inst[3], open)
		gxLink(t, d, inst[10], fixed) // older than 90d: ignored
		origin := gxOrigin(t, d)

		// Systemic origin + ladder actions over the 3 systemics above plus the origin.
		registered := "fixer"
		if _, _, err := d.RegisterAgent("p", "fixer-1", "dev", "", nil, &registered, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register: %v", err)
		}
		task, err := d.DispatchTask("p", "fixer", "lead", "fix dead lane", "", "P2", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		_, _ = d.conn.Exec(`UPDATE tasks SET status = 'done' WHERE id = ?`, task.ID)
		if _, err := d.conn.Exec(`INSERT INTO obligations (id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id,
			bearer_kind, bearer, state, created_at, discharge_evidence)
			VALUES ('ob1', 'p', 'exc.route_specialist', 1, 'h', 'exception', ?, 'owner', 'fixer-1', 'fulfilled', ?, ?)`,
			fixed, now.UTC().Format(memoryTimeFmt), `{"action_ref":"`+task.ID+`"}`); err != nil {
			t.Fatalf("obligation: %v", err)
		}
		sysOrigin := gxSystemic(t, d, "p", "dead_lane", "resolved", "fixed", "lead", now.Add(-10*day))

		// Checked-in expectation per action.
		want := map[string]GuardReplay{
			// 9 in window: inst[0,1] conflict (fixed systemic), inst[3] undecided (open), 6 agree.
			GuardActionSuppress: {Matched: 9, Agree: 6, Conflicts: 2, Undecided: 1},
			// systemics fixed / wont_fix / open vs reason wont_fix.
			GuardActionResolveSystemic: {Matched: 3, Agree: 1, Conflicts: 1, Undecided: 1},
			// fixed by fixer -> agree; wont_fix and open -> undecided.
			GuardActionRoute: {Matched: 3, Agree: 1, Conflicts: 0, Undecided: 2},
		}
		check := func(action string, g Guard) {
			t.Helper()
			r, w := g.Replay, want[action]
			if r.WindowDays != 90 || r.Matched != w.Matched || r.Agree != w.Agree || r.Conflicts != w.Conflicts || r.Undecided != w.Undecided {
				t.Fatalf("%s replay = %+v, want %+v", action, r, w)
			}
			if len(r.ConflictIDs) != r.Conflicts {
				t.Fatalf("%s conflict_ids = %v", action, r.ConflictIDs)
			}
		}
		check(GuardActionSuppress, gxMustCompile(t, d, origin, "dev-a", nil))
		check(GuardActionResolveSystemic, gxMustCompile(t, d, sysOrigin, "lead", func(in *GuardCompile) {
			in.Point, in.Action, in.ActionParams = GuardPointLadder, GuardActionResolveSystemic, map[string]string{"reason": "wont_fix"}
		}))
		check(GuardActionRoute, gxMustCompile(t, d, sysOrigin, "lead", func(in *GuardCompile) {
			in.Point, in.Action, in.ActionParams = GuardPointLadder, GuardActionRoute, map[string]string{"profile": "fixer"}
		}))
		_, err = gxCompile(d, sysOrigin, "lead", func(in *GuardCompile) {
			in.Point, in.Action, in.ActionParams = GuardPointLadder, GuardActionRoute, map[string]string{"profile": "ghost"}
		})
		gxCode(t, err, GuardErrBadAction)
	})

	t.Run("NoModelAtMatchTime", func(t *testing.T) {
		// The matcher, replay and lifecycle live in guards.go, which may import
		// the standard library only: no embedding, model client or network
		// package can reach match time. (go list -deps of package db would
		// include the sqlite driver, so the pin is on the file's imports.)
		f, err := parser.ParseFile(token.NewFileSet(), "guards.go", nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse guards.go: %v", err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first := strings.Split(path, "/")[0]; strings.Contains(first, ".") || strings.HasPrefix(path, "agent-relay") {
				t.Fatalf("guards.go imports %q: matcher must be stdlib + database/sql only", path)
			}
			if strings.HasPrefix(path, "net") {
				t.Fatalf("guards.go imports %q: no network at match time", path)
			}
		}
		// Same input on two fresh DBs -> same hits.
		var runs [][]string
		for i := 0; i < 2; i++ {
			d := budgetDB(t)
			g := gxMustCompile(t, d, gxOrigin(t, d), "dev-a", nil)
			var hits []string
			for j, s := range []gxSpec{{}, {code: "misrouted", kind: "routing"}, {project: "q"}, {}} {
				id := gxOpen(t, d, s)
				n := gxCount(t, d, `SELECT COUNT(*) FROM guard_hits WHERE guard_id = ? AND exception_id = ?`, g.ID, id)
				hits = append(hits, strconv.Itoa(j)+":"+strconv.Itoa(n))
			}
			runs = append(runs, hits)
		}
		if strings.Join(runs[0], ",") != strings.Join(runs[1], ",") || strings.Join(runs[0], ",") != "0:1,1:0,2:0,3:1" {
			t.Fatalf("hits differ or wrong: %v", runs)
		}
	})

	t.Run("NeverOverridesIdentityOrAuthGuards", func(t *testing.T) {
		d := budgetDB(t)
		for _, kind := range []string{"identity", "self_grading", "auth", "integrity"} {
			o := gxOpen(t, d, gxSpec{code: "impersonation", kind: kind, ago: 8 * 24 * time.Hour, resolvedBy: "self", reason: "fixed"})
			_, err := gxCompile(d, o, "dev-a", nil)
			gxCode(t, err, GuardErrProtectedClass)
		}
		// A systemic over identity instances is protected too.
		sys := gxSystemic(t, d, "p", "impersonation", "resolved", "fixed", "lead", time.Now().Add(-time.Hour))
		inst := gxOpen(t, d, gxSpec{code: "impersonation", kind: "identity"})
		gxLink(t, d, inst, sys)
		_, err := gxCompile(d, sys, "lead", func(in *GuardCompile) {
			in.Point, in.Action, in.ActionParams = GuardPointLadder, GuardActionResolveSystemic, map[string]string{"reason": "wont_fix"}
		})
		gxCode(t, err, GuardErrProtectedClass)
		if n := gxCount(t, d, `SELECT COUNT(*) FROM compiled_guards`); n != 0 {
			t.Fatalf("guards = %d, want 0", n)
		}
		// Even a guard row written behind the compiler's back never acts on it.
		if _, err := d.conn.Exec(`INSERT INTO compiled_guards (id, project, from_exception_id, point, action, scope_expr, specificity,
			mode, replay_json, created_by, source, created_at, expires_at)
			VALUES ('rogue', 'p', 'x', 'open', 'suppress', '{"kind":"identity","reason_code":"impersonation"}', 2, 'active', '{}',
			'x', 'agent_resolution', '2000-01-01T00:00:00.000000Z', '2999-01-01T00:00:00.000000Z')`); err != nil {
			t.Fatalf("rogue guard: %v", err)
		}
		gxOpen(t, d, gxSpec{code: "impersonation", kind: "identity"})
		if n := gxCount(t, d, `SELECT COUNT(*) FROM guard_hits`); n != 0 {
			t.Fatalf("guard_hits on a protected class = %d, want 0", n)
		}
	})

	t.Run("ShadowRecordsNeverActs", func(t *testing.T) {
		d := budgetDB(t)
		g := gxMustCompile(t, d, gxOrigin(t, d), "dev-a", nil)
		for i := 0; i < 3; i++ {
			gxOpen(t, d, gxSpec{ago: time.Duration(3-i) * time.Hour})
		}
		g = gxGuard(t, d, g.ID)
		if g.ShadowHits != 3 || g.LiveHits != 0 {
			t.Fatalf("shadow_hits=%d live_hits=%d, want 3/0", g.ShadowHits, g.LiveHits)
		}
		if n := gxCount(t, d, `SELECT COUNT(*) FROM guard_hits WHERE acted = 1`); n != 0 {
			t.Fatalf("acted hits = %d, want 0", n)
		}
		opened, err := d.EvaluateClassBudgets(time.Now())
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if len(opened) != 1 || opened[0].Instances != 3 {
			t.Fatalf("systemics = %+v, want one over the 3 shadow-hit instances", opened)
		}
	})

	t.Run("ActiveSuppressExcludesFromBudget", func(t *testing.T) {
		d := budgetDB(t)
		g := gxPromoted(t, d, nil)
		for i := 0; i < 5; i++ {
			gxOpen(t, d, gxSpec{ago: time.Duration(5-i) * time.Hour})
		}
		if g = gxGuard(t, d, g.ID); g.LiveHits != 5 {
			t.Fatalf("live_hits = %d, want 5", g.LiveHits)
		}
		for i := 0; i < 3; i++ {
			gxOpen(t, d, gxSpec{project: "q", ago: time.Duration(3-i) * time.Hour})
		}
		opened, err := d.EvaluateClassBudgets(time.Now())
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if len(opened) != 1 || opened[0].Project != "q" {
			t.Fatalf("systemics = %+v, want only q (p's 5 were suppressed)", opened)
		}
		if n := gxCount(t, d, `SELECT COUNT(*) FROM exceptions WHERE project = 'p' AND cause_id IS NOT NULL`); n != 0 {
			t.Fatalf("suppressed instances linked = %d, want 0", n)
		}
	})

	t.Run("PromotionNeedsDifferentAgentAndEvidence", func(t *testing.T) {
		d := budgetDB(t)
		o := gxOrigin(t, d) // resolver: dev-a (self)
		_, _ = d.conn.Exec(`UPDATE exceptions SET owner = 'lead' WHERE id = ?`, o)
		g := gxMustCompile(t, d, o, "lead", nil) // created_by: lead
		gxOpen(t, d, gxSpec{ago: 10 * 24 * time.Hour})
		gxOpen(t, d, gxSpec{ago: 11 * 24 * time.Hour})
		_, err := d.PromoteGuard(g.ID, "third", time.Now())
		gxCode(t, err, GuardErrPromoteRefused) // 2 hits
		conflicted := gxOpen(t, d, gxSpec{ago: 12 * 24 * time.Hour})
		for _, who := range []string{"lead", "dev-a"} {
			_, err := d.PromoteGuard(g.ID, who, time.Now())
			gxCode(t, err, GuardErrPromoteRefused)
		}
		// One shadow conflict: the third hit's systemic was repaired.
		gxLink(t, d, conflicted, gxSystemic(t, d, "p", "dead_lane", "resolved", "fixed", "lead", time.Now().Add(-time.Hour)))
		if _, err := d.SweepGuards(time.Now()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if g = gxGuard(t, d, g.ID); g.ShadowConflicts != 1 {
			t.Fatalf("shadow_conflicts = %d, want 1", g.ShadowConflicts)
		}
		_, err = d.PromoteGuard(g.ID, "third", time.Now())
		gxCode(t, err, GuardErrPromoteRefused)

		clean := budgetDB(t)
		p := gxPromoted(t, clean, nil)
		if p.Mode != GuardModeActive || p.ChallengedBy != "third" || p.AnchorAt == "" {
			t.Fatalf("promoted mode=%s challenged_by=%s anchor=%q", p.Mode, p.ChallengedBy, p.AnchorAt)
		}
		if n := gxCount(t, clean, `SELECT COUNT(*) FROM audit_log WHERE action = 'guard_promote' AND resource_id = ?`, p.ID); n != 1 {
			t.Fatalf("promote audit = %d, want 1", n)
		}
	})

	t.Run("PrecedenceMostSpecificActs", func(t *testing.T) {
		d := budgetDB(t)
		backend := "backend"
		if _, _, err := d.RegisterAgent("p", "dev-a", "dev", "", nil, &backend, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register: %v", err)
		}
		broad := gxPromoted(t, d, nil)
		narrow := gxPromoted(t, d, func(in *GuardCompile) {
			var cls string
			_ = d.conn.QueryRow(`SELECT class_id FROM exceptions WHERE id = ?`, in.ExceptionID).Scan(&cls)
			in.Scope = map[string]any{"class_id": cls, "project": "p", "raised_by_profile": "backend"}
		})
		id := gxOpen(t, d, gxSpec{ago: time.Hour})
		acted := func(g string) int {
			return gxCount(t, d, `SELECT COALESCE(SUM(acted), -1) FROM guard_hits WHERE guard_id = ? AND exception_id = ?`, g, id)
		}
		if acted(narrow.ID) != 1 || acted(broad.ID) != 0 {
			t.Fatalf("acted narrow=%d broad=%d, want 1/0", acted(narrow.ID), acted(broad.ID))
		}
		// Equal specificity with different actions: neither acts, both conflict.
		// (Only suppress exists at the open point in S1; differing params stand in.)
		_, _ = d.conn.Exec(`UPDATE compiled_guards SET action_params = '{"reason":"other"}', specificity = 3 WHERE id = ?`, broad.ID)
		id = gxOpen(t, d, gxSpec{ago: 30 * time.Minute})
		if acted(narrow.ID) != 0 || acted(broad.ID) != 0 {
			t.Fatalf("tie acted narrow=%d broad=%d, want 0/0", acted(narrow.ID), acted(broad.ID))
		}
		for _, g := range []string{narrow.ID, broad.ID} {
			if m := gxGuard(t, d, g).Mode; m != GuardModeShadow {
				t.Fatalf("conflicting active guard mode = %s, want shadow (demoted)", m)
			}
		}
	})

	t.Run("SuppressMassRegressionBackstop", func(t *testing.T) {
		d := budgetDB(t) // routing: intensity 3 per 7d -> backstop above 6
		g := gxPromoted(t, d, nil)
		var last string
		for i := 0; i < 7; i++ {
			last = gxOpen(t, d, gxSpec{ago: time.Duration(7-i) * time.Hour})
		}
		g = gxGuard(t, d, g.ID)
		if g.Mode != GuardModeShadow || g.LiveHits != 6 || g.Regressions != 1 {
			t.Fatalf("mode=%s live_hits=%d regressions=%d, want shadow/6/1", g.Mode, g.LiveHits, g.Regressions)
		}
		if n := gxCount(t, d, `SELECT acted FROM guard_hits WHERE guard_id = ? AND exception_id = ?`, g.ID, last); n != 0 {
			t.Fatalf("7th instance acted = %d, want 0 (counts again)", n)
		}
		if n := gxCount(t, d, `SELECT COUNT(*) FROM audit_log WHERE action = 'guard_demote' AND resource_id = ?`, g.ID); n != 1 {
			t.Fatalf("demote audit = %d, want 1", n)
		}
	})

	t.Run("RegressionDemotes", func(t *testing.T) {
		d := budgetDB(t)
		now := time.Now()
		fixer := "fixer"
		if _, _, err := d.RegisterAgent("p", "fixer-1", "dev", "", nil, &fixer, false, nil, "[]", 0, RegisterOptions{}); err != nil {
			t.Fatalf("register: %v", err)
		}
		origin := gxSystemic(t, d, "p", "dead_lane", "resolved", "fixed", "lead", now.Add(-100*time.Hour))
		g := gxMustCompile(t, d, origin, "lead", func(in *GuardCompile) {
			in.Point, in.Action, in.ActionParams = GuardPointLadder, GuardActionRoute, map[string]string{"profile": "fixer"}
		})
		// Ladder-point hits land with S2 (match_precedent); seed the evidence.
		_, _ = d.conn.Exec(`UPDATE compiled_guards SET shadow_hits = 3 WHERE id = ?`, g.ID)
		anchor := now.Add(-72 * time.Hour)
		if _, err := d.PromoteGuard(g.ID, "third", anchor); err != nil {
			t.Fatalf("promote: %v", err)
		}
		gxSystemic(t, d, "p", "dead_lane", "resolved", "wont_fix", "lead", anchor.Add(12*time.Hour)) // inside grace
		if _, err := d.SweepGuards(now); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if g = gxGuard(t, d, g.ID); g.Mode != GuardModeActive || g.Regressions != 0 {
			t.Fatalf("after in-grace recurrence mode=%s regressions=%d, want active/0", g.Mode, g.Regressions)
		}
		gxSystemic(t, d, "p", "dead_lane", "resolved", "wont_fix", "lead", now.Add(-10*time.Hour))
		gxSystemic(t, d, "p", "dead_lane", "open", "", "lead", now.Add(-5*time.Hour))
		sw, err := d.SweepGuards(now)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if g = gxGuard(t, d, g.ID); g.Mode != GuardModeShadow || g.Regressions != 2 || len(sw.Demoted) != 1 {
			t.Fatalf("mode=%s regressions=%d demoted=%v, want shadow/2/1", g.Mode, g.Regressions, sw.Demoted)
		}
		if n := gxCount(t, d, `SELECT COUNT(*) FROM audit_log WHERE action = 'guard_demote' AND resource_id = ?`, g.ID); n != 1 {
			t.Fatalf("demote audit = %d, want 1", n)
		}
		if sw, _ = d.SweepGuards(now); len(sw.Demoted) != 0 {
			t.Fatalf("second sweep demoted again: %v", sw.Demoted)
		}
	})

	t.Run("ExpireAndRetire", func(t *testing.T) {
		d := budgetDB(t)
		now := time.Now()
		short := gxPromoted(t, d, func(in *GuardCompile) { in.ExpiresIn = 24 * time.Hour })
		sw, err := d.SweepGuards(now.Add(25 * time.Hour))
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if gxGuard(t, d, short.ID).Mode != GuardModeExpired || len(sw.Expired) != 1 {
			t.Fatalf("mode=%s expired=%v, want expired", gxGuard(t, d, short.ID).Mode, sw.Expired)
		}
		id := gxOpen(t, d, gxSpec{})
		if n := gxCount(t, d, `SELECT COUNT(*) FROM guard_hits WHERE exception_id = ?`, id); n != 0 {
			t.Fatalf("expired guard hit = %d, want 0", n)
		}

		d2 := budgetDB(t)
		idle := gxMustCompile(t, d2, gxOrigin(t, d2), "dev-a", func(in *GuardCompile) { in.ExpiresIn = 90 * 24 * time.Hour })
		if sw, _ = d2.SweepGuards(now.Add(29 * 24 * time.Hour)); len(sw.Retired) != 0 {
			t.Fatalf("retired early: %v", sw.Retired)
		}
		if sw, _ = d2.SweepGuards(now.Add(31 * 24 * time.Hour)); len(sw.Retired) != 1 || gxGuard(t, d2, idle.ID).Mode != GuardModeRetired {
			t.Fatalf("retired=%v mode=%s, want retired", sw.Retired, gxGuard(t, d2, idle.ID).Mode)
		}
		id = gxOpen(t, d2, gxSpec{})
		if n := gxCount(t, d2, `SELECT COUNT(*) FROM guard_hits WHERE exception_id = ?`, id); n != 0 {
			t.Fatalf("retired guard hit = %d, want 0", n)
		}
	})

	t.Run("RenewNeedsOtherAgentAndLiveHits", func(t *testing.T) {
		d := budgetDB(t)
		g := gxPromoted(t, d, nil)
		_, err := d.RenewGuard(g.ID, "third", 30*24*time.Hour, time.Now())
		gxCode(t, err, GuardErrRenewRefused) // no live hit yet
		gxOpen(t, d, gxSpec{ago: time.Hour})
		_, err = d.RenewGuard(g.ID, "dev-a", 30*24*time.Hour, time.Now())
		gxCode(t, err, GuardErrRenewRefused) // creator
		_, err = d.RenewGuard(g.ID, "third", 91*24*time.Hour, time.Now())
		gxCode(t, err, GuardErrExpiryTooLong)
		r, err := d.RenewGuard(g.ID, "third", 60*24*time.Hour, time.Now())
		if err != nil || r.ExpiresAt <= g.ExpiresAt {
			t.Fatalf("renew: %v expires %s -> %s", err, g.ExpiresAt, r.ExpiresAt)
		}
	})

	t.Run("OpenTxAtomic", func(t *testing.T) {
		d := testDB(t)
		first := excStartedTask(t, d, "dev-a")
		excBlock(t, d, first, "dev-a", "waiting on fixture review")
		if _, err := d.StartTask(first, "dev-a", "p1"); err != nil {
			t.Fatalf("resume: %v", err)
		}
		origin := excRowsFor(t, d, first)[0].ID
		g := gxMustCompile(t, d, origin, "dev-a", nil)
		_, _ = d.conn.Exec(`UPDATE compiled_guards SET mode = 'active', promoted_at = ? WHERE id = ?`,
			time.Now().UTC().Format(memoryTimeFmt), g.ID)
		if _, err := d.conn.Exec(`DROP TABLE guard_hits`); err != nil {
			t.Fatalf("drop: %v", err)
		}
		id := excStartedTask(t, d, "dev-a")
		r := "waiting on fixture review"
		if _, err := d.BlockTask(id, "dev-a", "p1", &r); err == nil {
			t.Fatal("BlockTask succeeded with guard_hits gone, want an error")
		}
		task, err := d.GetTask(id, "p1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if task.Status != "in-progress" || len(excRowsFor(t, d, id)) != 0 {
			t.Fatalf("status=%s exceptions=%d after a failed guard write, want in-progress/0 (rolled back)",
				task.Status, len(excRowsFor(t, d, id)))
		}
	})
}
