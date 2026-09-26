package relay

import (
	"encoding/json"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Answer breach re-check (task fc67d019): at the deadline the breach re-checks
// message_answered in its own tx, so a bearer who already answered never
// escalates: by reply_to (a fulfil the send path missed) or by a message to
// the asker citing the ask id's 8-hex prefix without reply_to.

// breachEvidence returns the discharge_evidence of rec's obligation on ask.
func (f *answerFixture) breachEvidence(t *testing.T, ask string) map[string]string {
	t.Helper()
	var raw string
	if err := f.raw.QueryRow(`SELECT COALESCE(discharge_evidence, '') FROM obligations WHERE subject_id = ? AND bearer = 'rec'`, ask).Scan(&raw); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	ev := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("evidence %q: %v", raw, err)
	}
	return ev
}

// assertNoEscalation: rec's obligation is fulfilled, no rung opened, no
// sanction sent.
func (f *answerFixture) assertNoEscalation(t *testing.T, ask string) {
	t.Helper()
	if got := f.answers(t, ask); len(got) != 1 || got["rec"] != db.ObligationFulfilled {
		t.Fatalf("after the breach sweep: %v, want rec fulfilled only", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM obligations WHERE subject_id = ? AND norm_id = ?`, ask, db.NormAnswerRole); n != 0 {
		t.Fatalf("answer.role rows = %d, want 0", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE from_agent = 'relay'`); n != 0 {
		t.Fatalf("relay sanctions = %d, want 0", n)
	}
}

// TestCitedReplyWithoutReplyToFulfilsAtBreach (AC1): rec answers asker with
// "re <id8>: ..." and no reply_to; the breach sweep fulfils answer.reply with
// match=id_cite naming that reply, and opens no answer.role.
func TestCitedReplyWithoutReplyToFulfilsAtBreach(t *testing.T) {
	f := escalationFixture(t)
	ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask", "subject": "schema", "content": "drop column x?"})
	reply := f.send(t, map[string]any{"as": "rec", "to": "asker", "subject": "re " + ask[:8] + ": yes", "content": "drop it"})
	if got := f.answers(t, ask); got["rec"] != db.ObligationActive {
		t.Fatalf("a cite without reply_to fulfilled on send: %v (the send path is unchanged)", got)
	}

	at := time.Now().UTC().Add(61 * time.Minute)
	f.sweep(at)
	f.sweep(at.Add(time.Minute))
	f.assertNoEscalation(t, ask)
	if ev := f.breachEvidence(t, ask); ev["reply"] != reply || ev["match"] != "id_cite" {
		t.Fatalf("evidence = %v, want reply %s match id_cite", ev, reply)
	}

	// The cite in the content, not the subject, counts the same.
	ask2 := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "decide", "content": "A or B?"})
	f.send(t, map[string]any{"as": "rec", "to": "asker", "subject": "choice", "content": "on " + ask2[:8] + " I pick B"})
	f.sweep(time.Now().UTC().Add(61 * time.Minute))
	if got := f.answers(t, ask2); len(got) != 1 || got["rec"] != db.ObligationFulfilled {
		t.Fatalf("content cite: %v, want rec fulfilled only", got)
	}
}

// TestCiteFromWrongPartyStillEscalates (AC2): the same cite sent by a third
// agent to the asker, or by rec to someone other than the asker, does not
// answer the ask: the rung escalates as before.
func TestCiteFromWrongPartyStillEscalates(t *testing.T) {
	cases := map[string]func(f *answerFixture, ask string){
		"third agent to asker": func(f *answerFixture, ask string) {
			f.send(t, map[string]any{"as": "other", "to": "asker", "subject": "re " + ask[:8] + ": not mine"})
		},
		"bearer to someone else": func(f *answerFixture, ask string) {
			f.send(t, map[string]any{"as": "rec", "to": "other", "subject": "re " + ask[:8] + ": fyi"})
		},
		"asker citing its own ask": func(f *answerFixture, ask string) {
			f.send(t, map[string]any{"as": "asker", "to": "rec", "subject": "ping " + ask[:8]})
		},
	}
	for name, cite := range cases {
		t.Run(name, func(t *testing.T) {
			f := escalationFixture(t)
			if res, _ := f.h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "other"})); res.IsError {
				t.Fatalf("register other: %s", expectError(t, res))
			}
			ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask", "content": "drop column x?"})
			cite(f, ask)
			f.sweep(time.Now().UTC().Add(61 * time.Minute))
			if got := f.answers(t, ask); got["rec"] != db.ObligationUnfulfilled || got["mgr"] != db.ObligationActive || len(got) != 2 {
				t.Fatalf("after the breach: %v, want rec unfulfilled + mgr active", got)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE from_agent = 'relay' AND to_agent = 'mgr' AND reply_to = ?`, ask); n != 1 {
				t.Fatalf("sanctions to mgr = %d, want 1", n)
			}
		})
	}
}

// TestBreachRechecksReplyToAnswer (AC3): a reply_to reply that landed but
// whose fulfil was missed (written past the send path) is fulfilled at breach
// with match=reply_to, including one that reaches the ask through a chain.
func TestBreachRechecksReplyToAnswer(t *testing.T) {
	f := escalationFixture(t)
	ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask", "content": "drop column x?"})
	follow := f.send(t, map[string]any{"as": "asker", "to": "rec", "reply_to": ask, "content": "also y?"})
	// rec's reply lands without the send path's fulfil (e.g. a crash between
	// the insert and the fulfil).
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	if _, err := f.raw.Exec(`INSERT INTO messages (id, from_agent, to_agent, reply_to, subject, content, created_at, project)
		VALUES ('reply-1', 'rec', 'asker', ?, 'ok', 'both fine', ?, 'p1')`, follow, ts); err != nil {
		t.Fatalf("insert reply: %v", err)
	}
	if got := f.answers(t, ask); got["rec"] != db.ObligationActive {
		t.Fatalf("before the sweep: %v, want rec active", got)
	}
	f.sweep(time.Now().UTC().Add(61 * time.Minute))
	f.assertNoEscalation(t, ask)
	if ev := f.breachEvidence(t, ask); ev["reply"] != "reply-1" || ev["match"] != "reply_to" {
		t.Fatalf("evidence = %v, want reply reply-1 match reply_to", ev)
	}
}

// TestRoleRungRecheckCountsRecipientCite: once escalated, the role rung's
// breach also re-checks the original recipient, whose cited answer ends the
// chain before the human rung (as a late reply_to answer does on send).
func TestRoleRungRecheckCountsRecipientCite(t *testing.T) {
	f := escalationFixture(t)
	ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask", "content": "drop column x?"})
	t1 := time.Now().UTC().Add(61 * time.Minute)
	f.sweep(t1)
	if got := f.answers(t, ask); got["mgr"] != db.ObligationActive {
		t.Fatalf("after the first breach: %v, want mgr active", got)
	}
	f.send(t, map[string]any{"as": "rec", "to": "asker", "subject": "re " + ask[:8] + ": late yes"})
	f.sweep(t1.Add(121 * time.Minute))
	if got := f.answers(t, ask); got["rec"] != db.ObligationUnfulfilled || got["mgr"] != db.ObligationFulfilled || len(got) != 2 {
		t.Fatalf("after the role rung's deadline: %v, want rec unfulfilled, mgr fulfilled, no human rung", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE to_agent = 'user'`); n != 0 {
		t.Fatalf("messages to user = %d, want 0", n)
	}
}
