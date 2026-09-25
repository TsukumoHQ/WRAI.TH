package relay

import (
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// DEC-wraith-obligations-1 slice 2a (task 6b4369f0): the ACK checker retires
// quirks Q1 (notify after escalate), Q2 (push-only, lost when offline) and Q4
// (always the dispatcher) with an explicit escalation chain that ends at a
// human and never passes through one.

type chainMsg struct{ to, typ, priority, action, subject string }

// chainMessages lists the durable relay messages the ACK chain wrote, oldest first.
func chainMessages(t *testing.T, w *twin) []chainMsg {
	t.Helper()
	rows, err := w.raw.Query(`SELECT d.to_agent, m.type, m.priority, COALESCE(m.action_required, ''), m.subject
		FROM messages m JOIN deliveries d ON d.message_id = m.id
		WHERE m.from_agent = 'relay' ORDER BY m.created_at, m.rowid`)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []chainMsg
	for rows.Next() {
		var m chainMsg
		if err := rows.Scan(&m.to, &m.typ, &m.priority, &m.action, &m.subject); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func obligationRow(t *testing.T, w *twin, normID, taskID string) (state, bearerKind, bearer, evidence string) {
	t.Helper()
	if err := w.raw.QueryRow(`SELECT state, bearer_kind, bearer, COALESCE(discharge_evidence, '') FROM obligations
		WHERE norm_id = ? AND subject_id = ?`, normID, taskID).Scan(&state, &bearerKind, &bearer, &evidence); err != nil {
		t.Fatalf("obligation %s/%s: %v", normID, taskID, err)
	}
	return
}

func tick(w *twin) { evaluateObligations(w.d, w.rec, time.Now().UTC()) }

func register(t *testing.T, w *twin, name string, reportsTo *string, exec bool) {
	t.Helper()
	if _, _, err := w.d.RegisterAgent("p1", name, "", "", reportsTo, nil, exec, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

func TestObligationsChain(t *testing.T) {
	t.Run("Q1NoNotifyAfterEscalate", func(t *testing.T) {
		w := newTwin(t, "q1")
		seedTask("t1", "pending", ago(50*time.Minute))(t, w)
		tick(w)
		tick(w)
		tick(w)
		msgs := chainMessages(t, w)
		if len(msgs) != 1 || !strings.HasPrefix(msgs[0].subject, "ESCALATED: ") {
			t.Fatalf("messages = %+v, want only the escalate", msgs)
		}
		if state, _, _, ev := obligationRow(t, w, db.NormAckNotify, "t1"); state != db.ObligationInactive || !strings.Contains(ev, "ack.escalate") {
			t.Fatalf("notify obligation = %s %s, want inactive superseded by ack.escalate", state, ev)
		}
		var notified *string
		if err := w.raw.QueryRow(`SELECT ack_notified_at FROM tasks WHERE id = 't1'`).Scan(&notified); err != nil || notified != nil {
			t.Fatalf("ack_notified_at = %v (err %v), want NULL: no notify after the escalate", notified, err)
		}
	})

	t.Run("Q2DurableNotifyAndEscalate", func(t *testing.T) {
		w := newTwin(t, "q2") // the dispatcher "cto" has no session: offline
		seedTask("t1", "pending", ago(20*time.Minute))(t, w)
		tick(w)
		setSQL(`UPDATE tasks SET dispatched_at = ? WHERE id = 't1'`, ago(50*time.Minute))(t, w)
		tick(w)
		msgs := chainMessages(t, w)
		if len(msgs) != 2 {
			t.Fatalf("messages = %+v, want notify then escalate persisted", msgs)
		}
		if n := msgs[0]; n.to != "cto" || n.typ != "fyi" || n.priority != "P2" || n.action != "none" {
			t.Fatalf("notify = %+v, want fyi P2 no-wake to the dispatcher", n)
		}
		if e := msgs[1]; e.to != "cto" || e.priority != "P1" || e.action != "do" || !strings.HasPrefix(e.subject, "ESCALATED: ") {
			t.Fatalf("escalate = %+v, want a P1 do message to the dispatcher", e)
		}
		inbox, err := w.d.GetInbox("p1", "cto", true, 50)
		if err != nil || len(inbox) != 2 {
			t.Fatalf("offline dispatcher inbox = %d (err %v), want both notices waiting", len(inbox), err)
		}
	})

	t.Run("BearerAssigneeProfile", func(t *testing.T) {
		w := newTwin(t, "assigned")
		seedTask("t1", "pending", ago(20*time.Minute))(t, w)
		setSQL(`UPDATE tasks SET assigned_to = 'dev-1' WHERE id = 't1'`)(t, w)
		tick(w)
		if _, kind, bearer, _ := obligationRow(t, w, db.NormAckNotify, "t1"); kind != db.BearerAssigneeProfile || bearer != "dev" {
			t.Fatalf("bearer = %s/%s, want assignee_profile/dev", kind, bearer)
		}
		if msgs := chainMessages(t, w); len(msgs) != 1 || msgs[0].to != "cto" {
			t.Fatalf("sanction target = %+v, want the dispatcher", msgs)
		}
	})

	t.Run("BearerPoolWhenUnassigned", func(t *testing.T) {
		w := newTwin(t, "pool")
		seedTask("t1", "pending", ago(20*time.Minute))(t, w)
		tick(w)
		if _, kind, bearer, _ := obligationRow(t, w, db.NormAckNotify, "t1"); kind != db.BearerProfilePool || bearer != "dev" {
			t.Fatalf("bearer = %s/%s, want profile_pool/dev", kind, bearer)
		}
		// Any member of the pool discharges it by claiming.
		setSQL(`UPDATE tasks SET status = 'accepted', claimed_by = 'dev-7' WHERE id = 't1'`)(t, w)
		tick(w)
		if state, _, _, _ := obligationRow(t, w, db.NormAckEscalate, "t1"); state != db.ObligationFulfilled {
			t.Fatalf("after a pool member claimed: escalate = %s, want fulfilled", state)
		}
	})

	t.Run("Rung2Resolution", func(t *testing.T) {
		cases := []struct {
			name, want, rule string
			setup            func(t *testing.T, w *twin)
		}{
			{"ReportsToActive", "boss", "reports_to", func(t *testing.T, w *twin) {
				boss := "boss"
				register(t, w, "boss", nil, false)
				register(t, w, "cto", &boss, false)
			}},
			{"ReportsToInactiveFallsToExecutive", "chief", "executive", func(t *testing.T, w *twin) {
				boss := "boss"
				register(t, w, "boss", nil, false)
				setSQL(`UPDATE agents SET status = 'inactive' WHERE name = 'boss'`)(t, w)
				register(t, w, "cto", &boss, false)
				register(t, w, "chief", nil, true)
			}},
			{"NobodyFallsToFounder", "user", "founder", func(t *testing.T, w *twin) {
				register(t, w, "cto", nil, false)
			}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				w := newTwin(t, "rung2")
				c.setup(t, w)
				seedTask("t1", "pending", ago(100*time.Minute))(t, w)
				out := captureLinkageLog(t, func() { tick(w) })
				msgs := chainMessages(t, w)
				if len(msgs) != 1 || msgs[0].to != c.want || msgs[0].priority != "P1" {
					t.Fatalf("rung 2 messages = %+v, want one P1 to %s", msgs, c.want)
				}
				if n := strings.Count(out, "[obligations] rung=2 task=t1 rule="+c.rule+" target="+c.want); n != 1 {
					t.Fatalf("want one journal line naming rule %s, got:\n%s", c.rule, out)
				}
			})
		}
	})

	t.Run("HumanOnlyAtEnd", func(t *testing.T) {
		// With an executive: agent at rung 2, human at rung 3, then silence.
		w := newTwin(t, "human")
		register(t, w, "cto", nil, false)
		register(t, w, "chief", nil, true)
		seedTask("t1", "pending", ago(100*time.Minute))(t, w)
		tick(w)
		setSQL(`UPDATE tasks SET dispatched_at = ? WHERE id = 't1'`, ago(5*time.Hour))(t, w)
		tick(w)
		tick(w)
		msgs := chainMessages(t, w)
		if len(msgs) != 2 || msgs[0].to != "chief" || msgs[1].to != "user" {
			t.Fatalf("chain = %+v, want chief (rung 2) then user (rung 3) and nothing after", msgs)
		}

		// Rung 2 already landed on the founder: rung 3 never fires.
		w2 := newTwin(t, "human2")
		register(t, w2, "cto", nil, false)
		seedTask("t1", "pending", ago(100*time.Minute))(t, w2)
		tick(w2)
		setSQL(`UPDATE tasks SET dispatched_at = ? WHERE id = 't1'`, ago(5*time.Hour))(t, w2)
		tick(w2)
		msgs = chainMessages(t, w2)
		if len(msgs) != 1 || msgs[0].to != "user" {
			t.Fatalf("chain = %+v, want the founder once, as the last rung", msgs)
		}
		if state, _, _, _ := obligationRow(t, w2, db.NormAckHuman, "t1"); state != db.ObligationInactive {
			t.Fatalf("human rung = %s, want inactive once rung 2 reached the founder", state)
		}

		// Every chain the engine writes ends at the human, never passes one.
		for _, c := range [][]chainMsg{msgs, chainMessages(t, w)} {
			for i, m := range c {
				if m.to == "user" && i != len(c)-1 {
					t.Fatalf("human in the middle of the chain: %+v", c)
				}
			}
		}
	})

	t.Run("EscalatedBeforeChainSendsNothing", func(t *testing.T) {
		w := newTwin(t, "predeploy")
		// Escalated under the old checker a day ago: both legacy marks set.
		seedTask("old", "pending", ago(36*time.Hour))(t, w)
		setSQL(`UPDATE tasks SET ack_notified_at = ?, ack_escalated_at = ? WHERE id = 'old'`, ago(35*time.Hour), ago(35*time.Hour))(t, w)
		// Escalated by slice 1 (obligation closed) before the chain norms existed.
		seedTask("s1", "pending", ago(36*time.Hour))(t, w)
		setSQL(`INSERT INTO obligations (id, project, norm_id, norm_version, bindings_hash, subject_kind, subject_id, bearer_kind, bearer, state, created_at, closed_at, escalation_depth)
			VALUES ('s1-esc', 'p1', 'ack.escalate', 1, 'legacy-hash', 'task', 's1', 'assignee_profile', 'dev', 'unfulfilled', ?, ?, 1)`, ago(35*time.Hour), ago(35*time.Hour))(t, w)
		setSQL(`UPDATE tasks SET ack_escalated_at = NULL WHERE id = 's1'`)(t, w)
		tick(w)
		tick(w)
		if msgs := chainMessages(t, w); len(msgs) != 0 {
			t.Fatalf("pre-chain escalations fired %+v, want nothing", msgs)
		}
		// Slice 1 left s1's notify unopened; it must open superseded, not fire late (Q1).
		if state, _, _, ev := obligationRow(t, w, db.NormAckNotify, "s1"); state != db.ObligationInactive || !strings.Contains(ev, "superseded") {
			t.Fatalf("s1 notify = %s %s, want inactive superseded", state, ev)
		}
		for _, task := range []string{"old", "s1"} {
			for _, norm := range []string{db.NormAckManager, db.NormAckHuman} {
				if state, _, _, ev := obligationRow(t, w, norm, task); state != db.ObligationInactive || !strings.Contains(ev, "escalated before chain") {
					t.Fatalf("%s/%s = %s %s, want inactive 'escalated before chain'", norm, task, state, ev)
				}
			}
		}
	})
}
