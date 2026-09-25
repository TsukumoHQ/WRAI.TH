package relay

import (
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Answer obligations, slice B (task a01d0b87): past answer_reply_age an
// unanswered ask breaches to the recipient's role, past answer_role_age to the
// human (max depth only). Each rung sends one P1 message quoting the ask; the
// original delivery is never touched.

// escalationFixture: asker asks rec, whose reports_to is mgr.
func escalationFixture(t *testing.T) *answerFixture {
	t.Helper()
	f := newAnswerFixture(t, "asker", "mgr")
	if res, _ := f.h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "rec", "reports_to": "mgr"})); res.IsError {
		t.Fatalf("register rec: %s", expectError(t, res))
	}
	return f
}

func (f *answerFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.raw.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *answerFixture) sweep(at time.Time) {
	evaluateObligations(f.h.db, f.h.registry, at)
}

// TestAnswerBreachOpensRoleChildOnce (AC1): past answer_reply_age the next
// sweep breaches rec's obligation and opens exactly one answer.role on mgr
// (parent set) with exactly one P1 message quoting the ask; a second sweep
// opens nothing new.
func TestAnswerBreachOpensRoleChildOnce(t *testing.T) {
	f := escalationFixture(t)
	ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask", "subject": "schema", "content": "drop column x?"})
	recID := f.answerID(t, ask, "rec")

	f.sweep(time.Now().UTC().Add(30 * time.Minute)) // not due yet
	if got := f.answers(t, ask); len(got) != 1 || got["rec"] != db.ObligationActive {
		t.Fatalf("before the deadline: %v, want rec active only", got)
	}

	at := time.Now().UTC().Add(61 * time.Minute)
	f.sweep(at)
	f.sweep(at.Add(time.Minute))
	if got := f.answers(t, ask); got["rec"] != db.ObligationUnfulfilled || got["mgr"] != db.ObligationActive || len(got) != 2 {
		t.Fatalf("after the breach: %v, want rec unfulfilled + mgr active", got)
	}
	var norm, parent string
	var depth int
	if err := f.raw.QueryRow(`SELECT norm_id, COALESCE(parent_obligation_id, ''), escalation_depth FROM obligations WHERE subject_id = ? AND bearer = 'mgr'`, ask).
		Scan(&norm, &parent, &depth); err != nil || norm != db.NormAnswerRole || parent != recID || depth != 1 {
		t.Fatalf("mgr rung norm=%q parent=%q depth=%d (%v), want %s/%s/1", norm, parent, depth, err, db.NormAnswerRole, recID)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE from_agent = 'relay' AND to_agent = 'mgr' AND priority = 'P1'
		AND content LIKE '%drop column x?%' AND reply_to = ?`, ask); n != 1 {
		t.Fatalf("P1 messages to mgr quoting the ask = %d, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE from_agent = 'relay'`); n != 1 {
		t.Fatalf("relay messages = %d, want 1 (no second sanction)", n)
	}
}

// TestAnswerHumanOnlyAtMaxDepth (AC2): nothing reaches the user until the role
// rung breaches; then answer.human (depth 2) opens with one message to user,
// and the original ask's delivery to rec is unchanged throughout.
func TestAnswerHumanOnlyAtMaxDepth(t *testing.T) {
	f := escalationFixture(t)
	ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "decide", "content": "A or B?"})
	delivery := func() string {
		t.Helper()
		var state string
		if err := f.raw.QueryRow(`SELECT state || '|' || COALESCE(acknowledged_at, '') FROM deliveries WHERE message_id = ? AND to_agent = 'rec'`, ask).Scan(&state); err != nil {
			t.Fatalf("delivery: %v", err)
		}
		return state
	}
	before := delivery()
	toUser := `SELECT COUNT(*) FROM messages WHERE to_agent = 'user'`

	t1 := time.Now().UTC().Add(61 * time.Minute)
	f.sweep(t1)
	f.sweep(t1.Add(119 * time.Minute)) // role rung not due yet (7200s)
	if n := f.count(t, toUser); n != 0 {
		t.Fatalf("messages to user before max depth = %d, want 0", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM obligations WHERE subject_id = ? AND bearer = 'user'`, ask); n != 0 {
		t.Fatalf("human rung opened before the role rung breached")
	}

	f.sweep(t1.Add(121 * time.Minute))
	var depth int
	var state string
	if err := f.raw.QueryRow(`SELECT escalation_depth, state FROM obligations WHERE subject_id = ? AND norm_id = ? AND bearer = 'user'`, ask, db.NormAnswerHuman).
		Scan(&depth, &state); err != nil || depth != 2 || state != db.ObligationActive {
		t.Fatalf("human rung depth=%d state=%q (%v), want 2/active", depth, state, err)
	}
	if n := f.count(t, toUser); n != 1 {
		t.Fatalf("messages to user = %d, want 1", n)
	}
	f.sweep(t1.Add(24 * time.Hour)) // the human rung has no deadline: the chain stops
	if n := f.count(t, toUser); n != 1 {
		t.Fatalf("messages to user after a later sweep = %d, want still 1", n)
	}
	if after := delivery(); after != before {
		t.Fatalf("rec's delivery changed: %q -> %q", before, after)
	}
}

// TestAnswerRoleChildFulfilledAckUntouched (AC3): mgr's reply to the ask
// chain fulfils the role rung; a late reply from rec closes the rung opened for
// it; UnansweredAsks counts per recipient; ACK obligations and messages for a
// task are identical with and without answer traffic.
func TestAnswerRoleChildFulfilledAckUntouched(t *testing.T) {
	run := func(withAnswers bool) (acks, ackMsgs string) {
		f := escalationFixture(t)
		if _, err := f.raw.Exec(`INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, labels, blocked_periods, goal, acceptance_criteria, dod)
			VALUES ('t1', 'dev', 'asker', 'title', 'pending', 'p1', ?, '[]', '[]', '', '[]', '')`, ago(20*time.Minute)); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		start := time.Now().UTC().Add(-time.Minute)
		t1 := time.Now().UTC().Add(61 * time.Minute)
		// Both runs sweep at the same instants (t1, t1+3h); only the answer
		// traffic differs.
		if !withAnswers {
			f.sweep(t1)
			f.sweep(t1.Add(3 * time.Hour))
		} else {
			first := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask"})
			second := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask"})
			f.sweep(t1)

			counts, err := f.h.db.UnansweredAsks("p1", start)
			if err != nil {
				t.Fatalf("unanswered asks: %v", err)
			}
			got := map[string][2]int{}
			for _, u := range counts {
				got[u.Recipient] = [2]int{u.Active, u.Unfulfilled}
			}
			if got["rec"] != [2]int{0, 2} || got["mgr"] != [2]int{2, 0} {
				t.Fatalf("unanswered asks = %v, want rec 0/2, mgr 2/0", got)
			}
			res, _ := f.h.HandleObligationsMine(ctx, call(map[string]any{"project": "p1", "as": "mgr"}))
			if n := int(parseJSON(t, res)["count"].(float64)); n != 2 {
				t.Fatalf("obligations_mine for mgr = %d, want 2", n)
			}

			// mgr answers the escalation notice (itself a reply to the ask).
			var notice string
			if err := f.raw.QueryRow(`SELECT id FROM messages WHERE from_agent = 'relay' AND to_agent = 'mgr' AND reply_to = ?`, first).Scan(&notice); err != nil {
				t.Fatalf("notice: %v", err)
			}
			f.send(t, map[string]any{"as": "mgr", "to": "asker", "reply_to": notice, "content": "no"})
			if got := f.answers(t, first); got["mgr"] != db.ObligationFulfilled {
				t.Fatalf("after mgr's reply: %v, want mgr fulfilled", got)
			}
			// rec answers the second ask late: the rung opened for it closes too.
			f.send(t, map[string]any{"as": "rec", "to": "asker", "reply_to": second, "content": "late yes"})
			if got := f.answers(t, second); got["rec"] != db.ObligationUnfulfilled || got["mgr"] != db.ObligationFulfilled {
				t.Fatalf("after rec's late reply: %v, want rec unfulfilled (breached) and mgr fulfilled", got)
			}
			f.sweep(t1.Add(3 * time.Hour))
			if n := f.count(t, `SELECT COUNT(*) FROM messages WHERE to_agent = 'user' AND metadata LIKE '%"norm":"answer.%'`); n != 0 {
				t.Fatalf("answered asks still reached the user: %d message(s)", n)
			}
		}
		acks = ackRows(t, f.raw)
		rows, err := f.raw.Query(`SELECT to_agent || '|' || content FROM messages WHERE from_agent = 'relay' AND content LIKE '%title%' ORDER BY content`)
		if err != nil {
			t.Fatalf("ack messages: %v", err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			b.WriteString(s + "\n")
		}
		return acks, b.String()
	}
	acks0, msgs0 := run(false)
	acks1, msgs1 := run(true)
	if acks0 == "" || msgs0 == "" {
		t.Fatal("the ACK sweep produced nothing: the comparison would be vacuous")
	}
	if acks1 != acks0 || msgs1 != msgs0 {
		t.Fatalf("ACK side differs with answer traffic:\nobligations %q\n         vs %q\nmessages %q\n      vs %q", acks0, acks1, msgs0, msgs1)
	}
}
