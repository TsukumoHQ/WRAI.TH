package relay

import (
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Answer escalation walks the RECIPIENT's real reports_to chain and never
// lands on the asker (task b62b3966). Field case 2026-10-01: niwa-cto-2
// (executive, reports_to cto-tsukumo) asked cto-tsukumo (reports_to the
// non-agent 'founder'); the role rung fell through to "any active executive"
// and escalated the ask back to niwa-cto-2 as cto-tsukumo's lead.

func (f *answerFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.raw.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// roleBearer sweeps past answer_reply_age and returns the bearer of the
// rung opened at depth 1 (answer.role, or answer.human when the role
// resolved to the human).
func (f *answerFixture) roleBearer(t *testing.T, ask string) (bearer, norm string) {
	t.Helper()
	f.sweep(time.Now().UTC().Add(61 * time.Minute))
	if err := f.raw.QueryRow(`SELECT bearer, norm_id FROM obligations WHERE subject_id = ? AND escalation_depth >= 1 ORDER BY escalation_depth LIMIT 1`, ask).
		Scan(&bearer, &norm); err != nil {
		t.Fatalf("escalated rung: %v", err)
	}
	return bearer, norm
}

func (f *answerFixture) assertNoMessageTo(t *testing.T, agent, ask string) {
	t.Helper()
	if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE from_agent = 'relay' AND to_agent = ? AND reply_to = ?`, agent, ask); n != 0 {
		t.Fatalf("relay escalated the ask to %s %d time(s), want 0", agent, n)
	}
}

// TestAnswerEscalationClimbsRecipientChain (AC1): A reports_to B, B
// reports_to F; A's unanswered decide to B escalates to F (dev -> mid -> boss).
func TestAnswerEscalationClimbsRecipientChain(t *testing.T) {
	f := newAnswerFixture(t, "boss", "mid", "dev")
	f.exec(t, `UPDATE agents SET reports_to = 'boss' WHERE project = 'p1' AND name = 'mid'`)
	f.exec(t, `UPDATE agents SET reports_to = 'mid' WHERE project = 'p1' AND name = 'dev'`)
	ask := f.send(t, map[string]any{"as": "dev", "to": "mid", "action_required": "decide", "content": "A or B?"})

	if bearer, norm := f.roleBearer(t, ask); bearer != "boss" || norm != db.NormAnswerRole {
		t.Fatalf("role rung on %s (%s), want boss (%s)", bearer, norm, db.NormAnswerRole)
	}
	f.assertNoMessageTo(t, "dev", ask)
}

// TestAnswerEscalationSkipsSender (AC2): an escalation target equal to the
// asker is skipped. Field shape: the recipient reports_to a non-agent
// ('founder') and the asker is an active executive — the human gets the
// rung, never the asker. And when the recipient's lead IS the asker, the
// walk continues one rung up the chain.
func TestAnswerEscalationSkipsSender(t *testing.T) {
	t.Run("recipient reports_to a non-agent, asker is an executive", func(t *testing.T) {
		f := newAnswerFixture(t, "cto", "niwa2")
		f.exec(t, `UPDATE agents SET reports_to = 'founder', is_executive = 1 WHERE project = 'p1' AND name = 'cto'`)
		f.exec(t, `UPDATE agents SET reports_to = 'cto', is_executive = 1 WHERE project = 'p1' AND name = 'niwa2'`)
		ask := f.send(t, map[string]any{"as": "niwa2", "to": "cto", "action_required": "decide", "content": "ship it?"})

		if bearer, norm := f.roleBearer(t, ask); bearer != ackFounder || norm != db.NormAnswerHuman {
			t.Fatalf("escalated to %s (%s), want %s (%s)", bearer, norm, ackFounder, db.NormAnswerHuman)
		}
		f.assertNoMessageTo(t, "niwa2", ask)
	})
	t.Run("recipient's lead is the asker", func(t *testing.T) {
		f := newAnswerFixture(t, "boss", "lead", "dev")
		f.exec(t, `UPDATE agents SET reports_to = 'boss' WHERE project = 'p1' AND name = 'lead'`)
		f.exec(t, `UPDATE agents SET reports_to = 'lead' WHERE project = 'p1' AND name = 'dev'`)
		ask := f.send(t, map[string]any{"as": "lead", "to": "dev", "action_required": "decide", "content": "A or B?"})

		if bearer, norm := f.roleBearer(t, ask); bearer != "boss" || norm != db.NormAnswerRole {
			t.Fatalf("role rung on %s (%s), want boss (%s): the asker is skipped, next up", bearer, norm, db.NormAnswerRole)
		}
		f.assertNoMessageTo(t, "lead", ask)
	})
}

// TestAnswerEscalationInactiveReportOfSender (AC3): an inactive agent whose
// reports_to points at the asker does not make the asker the recipient's
// lead (field: inactive niwa-cto reports_to niwa-cto-2).
func TestAnswerEscalationInactiveReportOfSender(t *testing.T) {
	f := newAnswerFixture(t, "cto", "niwa2", "niwa1")
	f.exec(t, `UPDATE agents SET reports_to = 'founder', is_executive = 1 WHERE project = 'p1' AND name = 'cto'`)
	f.exec(t, `UPDATE agents SET reports_to = 'cto', is_executive = 1 WHERE project = 'p1' AND name = 'niwa2'`)
	f.exec(t, `UPDATE agents SET reports_to = 'niwa2', is_executive = 1, status = 'inactive' WHERE project = 'p1' AND name = 'niwa1'`)
	ask := f.send(t, map[string]any{"as": "niwa2", "to": "cto", "action_required": "decide", "content": "ship it?"})

	if bearer, norm := f.roleBearer(t, ask); bearer != ackFounder || norm != db.NormAnswerHuman {
		t.Fatalf("escalated to %s (%s), want %s (%s)", bearer, norm, ackFounder, db.NormAnswerHuman)
	}
	f.assertNoMessageTo(t, "niwa2", ask)
	f.assertNoMessageTo(t, "niwa1", ask)
}
