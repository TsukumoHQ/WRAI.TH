package relay

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"

	"github.com/google/uuid"
)

// Compiled guards at the ladder, slice S2b (task 8110485e): match_precedent
// evaluates ladder guards, a generalize=true human answer compiles a shadow
// guard, and the hourly sweep announces each demotion once.

// cloneSystemic copies the fixture's systemic into a new row with status /
// resolution_reason / owner / opened_at, no ladder, and returns its id.
func (f *ladderFixture) cloneSystemic(status, reason, owner string, opened time.Time) string {
	f.t.Helper()
	id := uuid.New().String()
	at := opened.UTC().Format("2006-01-02T15:04:05.000000Z")
	f.exec(`CREATE TEMP TABLE IF NOT EXISTS sys_clone AS SELECT * FROM exceptions WHERE 0`)
	f.exec(`DELETE FROM sys_clone`)
	f.exec(`INSERT INTO sys_clone SELECT * FROM exceptions WHERE id = ?`, f.sys)
	var resolvedBy, resolution, resolvedAt any
	if status == "resolved" {
		resolvedBy, resolution, resolvedAt = "peer", reason, at
	}
	f.exec(`UPDATE sys_clone SET id = ?, source_ref = ?, status = ?, resolved_by = ?, resolution_reason = ?, resolved_at = ?,
		owner = ?, opened_at = ?, rung = NULL, ladder_snapshot_json = NULL`,
		id, "clone|"+id, status, resolvedBy, resolution, resolvedAt, owner, at)
	f.exec(`INSERT INTO exceptions SELECT * FROM sys_clone`)
	return id
}

// ladderGuard compiles a ladder guard from a resolved clone owned by cmo and,
// when active, promotes it by raw write (promotion itself is S1/S2a's test).
func (f *ladderFixture) ladderGuard(action string, params map[string]string, active bool) string {
	f.t.Helper()
	origin := f.cloneSystemic("resolved", "fixed", "cmo", time.Now().Add(-10*24*time.Hour))
	g, err := f.db.CompileResolution(db.GuardCompile{ExceptionID: origin, Caller: "cmo", Point: db.GuardPointLadder,
		Action: action, ActionParams: params, ExpiresIn: 30 * 24 * time.Hour, Now: time.Now()})
	if err != nil {
		f.t.Fatalf("compile ladder guard: %v", err)
	}
	if active {
		at := time.Now().Add(-5 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000000Z")
		f.exec(`UPDATE compiled_guards SET mode = 'active', challenged_by = 'rev', promoted_at = ?, anchor_at = ? WHERE id = ?`, at, at, g.ID)
	}
	return g.ID
}

func (f *ladderFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.raw.QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func TestLadderGuards(t *testing.T) {
	t.Run("ActiveResolveSystemicAtRung0", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0")
		gid := f.ladderGuard(db.GuardActionResolveSystemic, map[string]string{"reason": "wont_fix"}, true)
		f.tick(time.Now())
		var status, by, reason string
		if err := f.raw.QueryRow(`SELECT status, resolved_by, resolution_reason FROM exceptions WHERE id = ?`, f.sys).Scan(&status, &by, &reason); err != nil {
			t.Fatal(err)
		}
		if status != "resolved" || by != "precedent" || reason != "wont_fix" {
			t.Fatalf("systemic %s by %s (%s), want resolved by precedent (wont_fix)", status, by, reason)
		}
		if n := f.count(`SELECT COUNT(*) FROM messages WHERE subject LIKE '%is it fixed?%'`); n != 0 || len(f.rec.drain()) != 0 {
			t.Fatalf("ask_source messages = %d, want none", n)
		}
		if n := f.count(`SELECT live_hits FROM compiled_guards WHERE id = ?`, gid); n != 1 {
			t.Fatalf("live_hits = %d, want 1", n)
		}
	})

	t.Run("ShadowRecordsOnly", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0")
		gid := f.ladderGuard(db.GuardActionResolveSystemic, map[string]string{"reason": "wont_fix"}, false)
		f.tick(time.Now())
		if rung, status, _ := f.state(); status != "open" || rung != db.RungConsultKnowledge {
			t.Fatalf("after one tick: rung %q status %s, want consult_knowledge / open as on main", rung, status)
		}
		if n := f.count(`SELECT COUNT(*) FROM guard_hits WHERE guard_id = ? AND exception_id = ? AND acted = 0 AND mode = 'shadow'`, gid, f.sys); n != 1 {
			t.Fatalf("shadow hits on the systemic = %d, want 1", n)
		}
		if n := f.count(`SELECT shadow_hits FROM compiled_guards WHERE id = ?`, gid); n != 1 {
			t.Fatalf("shadow_hits = %d, want 1", n)
		}
	})

	t.Run("ActiveRouteSkipsAskSource", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0", "p/spec/active/spec-lane//0")
		f.ladderGuard(db.GuardActionRoute, map[string]string{"profile": "spec-lane"}, true)
		f.tick(time.Now())
		if rung, status, _ := f.state(); status != "open" || rung != db.RungRouteSpecialist {
			t.Fatalf("after the guard: rung %q status %s, want route_specialist", rung, status)
		}
		if n := f.count(`SELECT COUNT(*) FROM obligations WHERE subject_id = ? AND norm_id IN ('exc.consult_knowledge', 'exc.ask_source')
			AND state = 'inactive' AND discharge_evidence LIKE '%guard_route%'`, f.sys); n != 2 {
			t.Fatalf("skipped rungs recorded = %d, want consult_knowledge + ask_source", n)
		}
		f.tick(time.Now())
		var profile string
		if err := f.raw.QueryRow(`SELECT profile_slug FROM tasks WHERE title LIKE '[exc %'`).Scan(&profile); err != nil || profile != "spec-lane" {
			t.Fatalf("route_specialist task profile = %q (%v), want spec-lane", profile, err)
		}
		if n := f.count(`SELECT COUNT(*) FROM messages WHERE subject LIKE '%is it fixed?%'`); n != 0 {
			t.Fatalf("ask_source messages = %d, want none", n)
		}
	})

	t.Run("HumanGeneralizeCompilesShadow", func(t *testing.T) {
		for _, generalize := range []bool{true, false} {
			f := newLadderFixture(t, "cmo", `[{"rung":"consult_knowledge"},{"rung":"human"}]`, "p/cmo/active/cmo-lane//0",
				"p/dev-0/active/dev//0", "p/dev-1/active/dev//0", "p/dev-2/active/dev//0")
			f.until(db.RungHuman)
			f.tick(time.Now())
			f.reply("user", f.actionRef(), fmt.Sprintf(`{"decision":"accept_known","scope":"lane","generalize":%t,"expires_in":"30d"}`, generalize))
			f.tick(time.Now())
			if _, status, by := f.state(); status != "resolved" || by != "human" {
				t.Fatalf("generalize=%t: %s by %s", generalize, status, by)
			}
			n := f.count(`SELECT COUNT(*) FROM compiled_guards`)
			if !generalize {
				if n != 0 {
					t.Fatalf("generalize=false compiled %d guards, want 0", n)
				}
				continue
			}
			var id, point, action, mode, source, createdBy, from, scope, expires string
			if err := f.raw.QueryRow(`SELECT id, point, action, mode, source, created_by, from_exception_id, scope_expr, expires_at FROM compiled_guards`).
				Scan(&id, &point, &action, &mode, &source, &createdBy, &from, &scope, &expires); err != nil || n != 1 {
				t.Fatalf("guards = %d (%v), want 1", n, err)
			}
			if point != db.GuardPointOpen || action != db.GuardActionSuppress || mode != db.GuardModeShadow ||
				source != db.GuardSourceHuman || createdBy != "human" || from != f.sys {
				t.Fatalf("guard %s: %s/%s %s source %s by %s from %s", id, point, action, mode, source, createdBy, from)
			}
			var class string
			_ = f.raw.QueryRow(`SELECT class_id FROM exceptions WHERE cause_id = ? LIMIT 1`, f.sys).Scan(&class)
			for _, want := range []string{`"class_id":"` + class + `"`, `"project":"p"`, `"raised_by_profile":"dev"`} {
				if !strings.Contains(scope, want) {
					t.Fatalf("scope %s lacks %s", scope, want)
				}
			}
			exp, _ := time.Parse("2006-01-02T15:04:05.000000Z", expires)
			if d := time.Until(exp); d < 29*24*time.Hour || d > 30*24*time.Hour {
				t.Fatalf("expires_at %s is %s away, want 30d", expires, d)
			}
		}
	})

	t.Run("DemotionNotifiesOnce", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0")
		gid := f.ladderGuard(db.GuardActionRoute, map[string]string{"profile": "cmo-lane"}, true)
		// Two matching systemics opened after anchor + grace (the fixture's and
		// a later one, since resolved): the repair did not take, two regressions demote.
		f.cloneSystemic("resolved", "known", "cmo", time.Now())
		now := time.Now()
		evaluateGuards(f.db, f.rec, now)
		evaluateGuards(f.db, f.rec, now.Add(2*GuardSweepInterval))
		if g, err := f.db.GetGuard(gid); err != nil || g.Mode != db.GuardModeShadow {
			t.Fatalf("guard %+v (%v), want demoted to shadow", g, err)
		}
		if n := f.count(`SELECT COUNT(*) FROM messages WHERE from_agent = 'relay' AND subject LIKE 'guard % demoted to shadow'`); n != 1 {
			t.Fatalf("demotion messages = %d across two ticks, want 1", n)
		}
		var to string
		if err := f.raw.QueryRow(`SELECT GROUP_CONCAT(d.to_agent) FROM deliveries d JOIN messages m ON m.id = d.message_id
			WHERE m.subject LIKE 'guard % demoted to shadow' ORDER BY d.to_agent`).Scan(&to); err != nil ||
			!strings.Contains(to, "cmo") || !strings.Contains(to, "rev") {
			t.Fatalf("demotion delivered to %q (%v), want creator cmo + challenger rev", to, err)
		}
	})

	t.Run("NeverRelaxesIdentityGuard", func(t *testing.T) {
		f := newLadderFixture(t, "cmo", "", "p/cmo/active/cmo-lane//0")
		gid := f.ladderGuard(db.GuardActionResolveSystemic, map[string]string{"reason": "wont_fix"}, true)
		f.exec(`UPDATE exceptions SET kind = 'identity' WHERE cause_id = ?`, f.sys)
		f.tick(time.Now())
		if rung, status, _ := f.state(); status != "open" || rung != db.RungConsultKnowledge {
			t.Fatalf("identity systemic: rung %q status %s, want the plain ladder", rung, status)
		}
		if n := f.count(`SELECT COUNT(*) FROM guard_hits WHERE guard_id = ?`, gid); n != 0 {
			t.Fatalf("guard hit an identity systemic %d time(s)", n)
		}
		// Nor can one be compiled from it.
		protected := f.cloneSystemic("resolved", "wont_fix", "cmo", time.Now())
		f.exec(`UPDATE exceptions SET cause_id = ? WHERE id = (SELECT id FROM exceptions WHERE cause_id = ? LIMIT 1)`, protected, f.sys)
		_, err := f.db.CompileResolution(db.GuardCompile{ExceptionID: protected, Caller: "cmo", Point: db.GuardPointLadder,
			Action: db.GuardActionResolveSystemic, ActionParams: map[string]string{"reason": "wont_fix"}, ExpiresIn: 30 * 24 * time.Hour})
		if err == nil || !strings.Contains(err.Error(), db.GuardErrProtectedClass) {
			t.Fatalf("compile from an identity systemic: %v, want %s", err, db.GuardErrProtectedClass)
		}
	})
}
