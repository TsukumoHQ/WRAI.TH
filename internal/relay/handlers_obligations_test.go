package relay

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// DEC-wraith-obligations-1 slice 2b (task 79f48b9e): obligations_mine,
// obligation_discharge (predicate re-checked) and obligation_decline
// (closed reason classes, early escalation).

type oblFixture struct {
	h   *Handlers
	raw *sql.DB
}

// newOblFixture seeds dev-1 (profile dev) and tasks, then runs one ACK sweep so
// the obligations exist.
func newOblFixture(t *testing.T) *oblFixture {
	t.Helper()
	d, path := memDB(t)
	h := memHandlersAt(t, d)
	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	dev := "dev"
	if _, _, err := d.RegisterAgent("p1", "dev-1", "", "", nil, &dev, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	f := &oblFixture{h: h, raw: raw}
	for _, tk := range []struct{ id, profile, assigned string }{
		{"pool", "dev", ""},        // unassigned: dev's pool
		{"mine", "dev", "dev-1"},   // assigned to the caller
		{"theirs", "dev", "dev-2"}, // assigned to someone else
		{"ops", "ops", ""},         // another profile's pool
	} {
		f.exec(t, `INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, assigned_to, labels, blocked_periods, goal, acceptance_criteria, dod)
			VALUES (?, ?, 'cto', ?, 'pending', 'p1', ?, NULLIF(?, ''), '[]', '[]', '', '[]', '')`, tk.id, tk.profile, "title "+tk.id, ago(20*time.Minute), tk.assigned)
	}
	evaluateObligations(d, h.registry, time.Now().UTC())
	return f
}

func (f *oblFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.raw.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *oblFixture) obligationID(t *testing.T, task, norm string) string {
	t.Helper()
	var id string
	if err := f.raw.QueryRow(`SELECT id FROM obligations WHERE subject_id = ? AND norm_id = ?`, task, norm).Scan(&id); err != nil {
		t.Fatalf("obligation %s/%s: %v", task, norm, err)
	}
	return id
}

func (f *oblFixture) state(t *testing.T, id string) (state, declineReason string) {
	t.Helper()
	if err := f.raw.QueryRow(`SELECT state, COALESCE(decline_reason_class, '') FROM obligations WHERE id = ?`, id).Scan(&state, &declineReason); err != nil {
		t.Fatalf("state %s: %v", id, err)
	}
	return
}

func TestObligationTools(t *testing.T) {
	t.Run("ObligationsMineIncludesPool", func(t *testing.T) {
		f := newOblFixture(t)
		res, _ := f.h.HandleObligationsMine(ctx, call(map[string]any{"project": "p1", "as": "dev-1"}))
		obs := parseJSON(t, res)["obligations"].([]any)
		seen := map[string]bool{}
		for _, o := range obs {
			m := o.(map[string]any)
			seen[m["subject_id"].(string)] = true
			if m["what"] != db.WhatTaskLeftPending || m["deadline"] == "" || m["subject_kind"] != "task" {
				t.Fatalf("obligation lacks what/deadline/subject: %v", m)
			}
			if _, ok := m["escalation_depth"]; !ok {
				t.Fatalf("obligation lacks escalation_depth: %v", m)
			}
		}
		if !seen["pool"] || !seen["mine"] || seen["theirs"] || seen["ops"] {
			t.Fatalf("mine = %v, want the pool task and the task assigned to dev-1 only", seen)
		}
		// Rung 0 already fired in the sweep: 3 active rungs per owed task.
		if len(obs) != 6 {
			t.Fatalf("got %d active obligations, want 6 (3 open rungs x 2 tasks)", len(obs))
		}
	})

	t.Run("DischargeRechecksPredicate", func(t *testing.T) {
		f := newOblFixture(t)
		id := f.obligationID(t, "pool", db.NormAckEscalate)
		f.exec(t, `UPDATE tasks SET status = 'accepted' WHERE id = 'pool'`) // taken up, sweeper not run yet
		res, _ := f.h.HandleObligationDischarge(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "id": id, "evidence": "claimed"}))
		if got := parseJSON(t, res)["state"]; got != db.ObligationFulfilled {
			t.Fatalf("discharge result state = %v", got)
		}
		if s, _ := f.state(t, id); s != db.ObligationFulfilled {
			t.Fatalf("stored state = %s, want fulfilled", s)
		}
	})

	t.Run("DischargeRefusedWhenPredicateFalse", func(t *testing.T) {
		f := newOblFixture(t)
		id := f.obligationID(t, "pool", db.NormAckEscalate)
		res, _ := f.h.HandleObligationDischarge(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "id": id, "evidence": "trust me"}))
		if msg := expectError(t, res); !strings.Contains(msg, "task_left_pending is false") {
			t.Fatalf("error = %s", msg)
		}
		if s, _ := f.state(t, id); s != db.ObligationActive {
			t.Fatalf("state = %s after a refused discharge, want active", s)
		}
	})

	t.Run("DeclineRequiresReasonClass", func(t *testing.T) {
		f := newOblFixture(t)
		id := f.obligationID(t, "pool", db.NormAckEscalate)
		for _, bad := range []string{"", "lazy"} {
			res, _ := f.h.HandleObligationDecline(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "id": id, "reason_class": bad}))
			if msg := expectError(t, res); !strings.Contains(msg, "reason_class must be one of") {
				t.Fatalf("reason_class %q: error = %s", bad, msg)
			}
		}
		if s, _ := f.state(t, id); s != db.ObligationActive {
			t.Fatalf("state = %s after refused declines, want active", s)
		}

		// Not the bearer: refused.
		other := f.obligationID(t, "ops", db.NormAckEscalate)
		res, _ := f.h.HandleObligationDecline(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "id": other, "reason_class": "not_mine"}))
		if msg := expectError(t, res); !strings.Contains(msg, "only the obligation's bearer") {
			t.Fatalf("non-bearer decline: %s", msg)
		}

		// The bearer declines: breach now, reason recorded, sanction sent now.
		res, _ = f.h.HandleObligationDecline(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "id": id, "reason_class": "blocked_by", "reason": "needs the schema ruling"}))
		out := parseJSON(t, res)
		if out["state"] != db.ObligationUnfulfilled || out["escalated_to"] != "cto" {
			t.Fatalf("decline result = %v", out)
		}
		if s, r := f.state(t, id); s != db.ObligationUnfulfilled || r != "blocked_by" {
			t.Fatalf("stored = %s/%s, want unfulfilled/blocked_by", s, r)
		}
		var subject string
		if err := f.raw.QueryRow(`SELECT subject FROM messages WHERE from_agent = 'relay' AND to_agent = 'cto' AND subject LIKE 'ESCALATED:%'`).Scan(&subject); err != nil {
			t.Fatalf("escalation message: %v", err)
		}
		if !strings.Contains(subject, "Declined by dev-1 (blocked_by: needs the schema ruling)") {
			t.Fatalf("escalation text = %q", subject)
		}
	})

	t.Run("SessionContextUnchanged", func(t *testing.T) {
		f := newOblFixture(t)
		sc := f.h.buildSessionContext("p1", "dev-1", nil)
		for k := range sc {
			if strings.Contains(k, "obligation") {
				t.Fatalf("session_context gained %q (surfacing is OUT)", k)
			}
		}
	})
}
