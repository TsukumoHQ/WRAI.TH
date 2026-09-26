package db

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// startedSystemic seeds 3 dead_lane instances, opens the systemic and returns
// it (ladder not started).
func startedSystemic(t *testing.T, d *DB) SystemicRef {
	t.Helper()
	seedDeadLane(t, d, 3)
	if _, err := d.EvaluateClassBudgets(time.Now()); err != nil {
		t.Fatal(err)
	}
	p, err := d.PendingSystemics()
	if err != nil || len(p) != 1 {
		t.Fatalf("pending systemics: %+v %v", p, err)
	}
	if p[0].Kind != "routing" {
		t.Fatalf("systemic kind = %q, want the instances' kind routing", p[0].Kind)
	}
	return p[0]
}

func activeRung(t *testing.T, d *DB) ActiveRung {
	t.Helper()
	rs, err := d.ActiveRungs()
	if err != nil || len(rs) != 1 {
		t.Fatalf("active rungs: %+v %v", rs, err)
	}
	return rs[0]
}

func TestExceptionLadder(t *testing.T) {
	t.Run("SnapshotFrozen", func(t *testing.T) {
		d := budgetDB(t)
		s := startedSystemic(t, d)
		if ok, err := d.StartLadder(s, time.Now()); !ok || err != nil {
			t.Fatalf("start: %v %v", ok, err)
		}
		if ok, _ := d.StartLadder(s, time.Now()); ok {
			t.Fatal("second start must be a no-op (CAS on rung IS NULL)")
		}
		if _, err := d.writerExec(`UPDATE class_budgets SET ladder_json = '[{"rung":"human"}]' WHERE kind = 'routing' AND reason_code = '*'`); err != nil {
			t.Fatal(err)
		}
		a := activeRung(t, d)
		if len(a.Snapshot.Rungs) != len(defaultLadder) || a.Snapshot.Source != "default" {
			t.Fatalf("running ladder changed after edit: %+v", a.Snapshot)
		}
	})

	t.Run("InvalidLadderInert", func(t *testing.T) {
		d := budgetDB(t)
		s := startedSystemic(t, d)
		if _, err := d.writerExec(`UPDATE class_budgets SET ladder_json = '[{"rung":"human"},{"rung":"supervisor_agent"}]' WHERE kind = 'routing' AND reason_code = '*'`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.StartLadder(s, time.Now()); !errors.Is(err, ErrLadderInvalid) {
			t.Fatalf("human not last: err = %v, want ErrLadderInvalid", err)
		}
		var enabled, audits, obligations int
		var rung *int
		_ = d.ro().QueryRow(`SELECT enabled FROM class_budgets WHERE kind = 'routing' AND reason_code = '*'`).Scan(&enabled)
		_ = d.ro().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'ladder_invalid'`).Scan(&audits)
		_ = d.ro().QueryRow(`SELECT rung FROM exceptions WHERE id = ?`, s.ID).Scan(&rung)
		_ = d.ro().QueryRow(`SELECT COUNT(*) FROM obligations WHERE subject_kind = 'exception'`).Scan(&obligations)
		if enabled != 0 || audits != 1 || rung != nil || obligations != 0 {
			t.Fatalf("invalid ladder not inert: enabled=%d audits=%d rung=%v obligations=%d", enabled, audits, rung, obligations)
		}
	})

	t.Run("CASDoubleAdvance", func(t *testing.T) {
		d := budgetDB(t)
		s := startedSystemic(t, d)
		_, _ = d.StartLadder(s, time.Now())
		a := activeRung(t, d)
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := d.AdvanceRung(a, ObligationFulfilled, nil, a.Rung+1, time.Now())
				if err != nil {
					t.Errorf("advance: %v", err)
				}
				if ok {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		var rung, children int
		_ = d.ro().QueryRow(`SELECT rung FROM exceptions WHERE id = ?`, s.ID).Scan(&rung)
		_ = d.ro().QueryRow(`SELECT COUNT(*) FROM obligations WHERE subject_id = ? AND escalation_depth = 1`, s.ID).Scan(&children)
		if wins != 1 || rung != 1 || children != 1 {
			t.Fatalf("double advance: wins=%d rung=%d children=%d, want 1/1/1", wins, rung, children)
		}
	})

	t.Run("HumanOnlyAtMaxDepth", func(t *testing.T) {
		d := budgetDB(t)
		s := startedSystemic(t, d)
		_, _ = d.StartLadder(s, time.Now())
		for {
			a := activeRung(t, d)
			if a.Name() == RungHuman {
				if a.Rung != len(a.Snapshot.Rungs)-1 {
					t.Fatalf("human at depth %d of %d", a.Rung, len(a.Snapshot.Rungs))
				}
				var open int
				_ = d.ro().QueryRow(`SELECT COUNT(*) FROM obligations WHERE subject_id = ? AND state = 'active' AND escalation_depth < ?`,
					s.ID, a.Rung).Scan(&open)
				if open != 0 {
					t.Fatalf("%d agent rungs still active when the human rung opened", open)
				}
				return
			}
			if ok, err := d.AdvanceRung(a, ObligationUnfulfilled, nil, a.Rung+1, time.Now()); !ok || err != nil {
				t.Fatalf("advance %s: %v %v", a.Name(), ok, err)
			}
		}
	})

	t.Run("UpstreamTrim", func(t *testing.T) {
		d := budgetDB(t)
		for i := 0; i < 3; i++ {
			id := "gate-" + string(rune('a'+i))
			seedTask(t, d.conn, id, "p", "blocked", "cto", "dev", "", "dev", "", "", false)
			if _, err := d.conn.Exec(`UPDATE tasks SET blocked_reason = 'rejected 3 rounds — human needed (reviewer rounds 2/2)' WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
			seedInstance(t, d, "p", "niwa_gate_rounds_exhausted", "gate_exhausted", "non_retryable", id, time.Duration(3-i)*time.Hour)
		}
		if _, err := d.EvaluateClassBudgets(time.Now()); err != nil {
			t.Fatal(err)
		}
		p, _ := d.PendingSystemics()
		if ok, err := d.StartLadder(p[0], time.Now()); !ok || err != nil {
			t.Fatalf("start: %v %v", ok, err)
		}
		a := activeRung(t, d)
		if a.Snapshot.UpstreamAttempts != 9 || a.Snapshot.Index(RungAskSource) != -1 || len(a.Snapshot.Trims) != 1 {
			t.Fatalf("upstream trim: %+v", a.Snapshot)
		}
	})

	t.Run("NoGateRerun", func(t *testing.T) {
		rungs := []LadderRung{
			{Rung: RungRouteSpecialist},
			{Rung: RungReversibleAction, Params: map[string]string{"tool": "qa-submit", "compensate": "none"}},
			{Rung: RungHuman},
		}
		if err := ValidateLadder(rungs, "reversible"); !errors.Is(err, ErrLadderInvalid) {
			t.Fatalf("reversible_action naming qa-submit accepted: %v", err)
		}
		rungs[1].Params["tool"] = "git-stash-guard"
		if err := ValidateLadder(rungs, "none"); !errors.Is(err, ErrLadderInvalid) {
			t.Fatalf("reversible_action on reversibility none accepted: %v", err)
		}
		if err := ValidateLadder(rungs, "reversible"); err != nil {
			t.Fatalf("valid reversible ladder refused: %v", err)
		}
		if err := ValidateLadder([]LadderRung{{Rung: RungAdversarialReview}, {Rung: RungHuman}}, "none"); !errors.Is(err, ErrLadderInvalid) {
			t.Fatal("adversarial_review without route_specialist accepted")
		}
	})

	t.Run("AttemptsView", func(t *testing.T) {
		d := budgetDB(t)
		s := startedSystemic(t, d)
		_, _ = d.StartLadder(s, time.Now())
		a := activeRung(t, d)
		_, _ = d.AdvanceRung(a, ObligationFulfilled, nil, 1, time.Now())
		a = activeRung(t, d)
		_, _ = d.AdvanceRung(a, ObligationInactive, map[string]any{"skipped": "x"}, 2, time.Now())
		a = activeRung(t, d)
		_, _ = d.ResolveAtRung(a, "source", "fixed", nil, time.Now())
		rows, err := d.ro().Query(`SELECT rung, strategy, outcome FROM exception_attempts WHERE exception_id = ? ORDER BY rung`, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		want := []string{"match_precedent/resolved_or_done", "consult_knowledge/skipped", "ask_source/resolved_or_done"}
		i := 0
		for rows.Next() {
			var rung int
			var strategy, outcome string
			_ = rows.Scan(&rung, &strategy, &outcome)
			if i >= len(want) || strategy+"/"+outcome != want[i] {
				t.Fatalf("attempt %d = %s/%s, want %v", i, strategy, outcome, want)
			}
			i++
		}
		if i != len(want) {
			t.Fatalf("attempts = %d, want %d", i, len(want))
		}
		var status, by string
		_ = d.ro().QueryRow(`SELECT status, resolved_by FROM exceptions WHERE id = ?`, s.ID).Scan(&status, &by)
		if status != "resolved" || by != "source" {
			t.Fatalf("systemic %s by %s", status, by)
		}
	})
}
