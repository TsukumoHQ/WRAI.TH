package relay

import (
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Answer on the same task (task 8f62ecb7): an ask tied to task T is answered
// when its bearer sends the asker any message tied to T, reply_to or not, so
// the lead is never escalated a question its doer already answered. The
// escalation notice quotes the bearer's last message to the asker after the
// ask, so the lead sees what was already said.

// seedTask inserts task id in p1 so a send's task_id resolves to it.
func (f *answerFixture) seedTask(t *testing.T, id string) {
	t.Helper()
	if _, err := f.raw.Exec(`INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, labels, blocked_periods, goal, acceptance_criteria, dod)
		VALUES (?, 'dev', 'rec', 'title', 'accepted', 'p1', ?, '[]', '[]', '', '[]', '')`, id, ago(time.Minute)); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// sanctionTo returns the content of the relay's answer notice to bearer on ask.
func (f *answerFixture) sanctionTo(t *testing.T, bearer, ask string) string {
	t.Helper()
	var body string
	if err := f.raw.QueryRow(`SELECT content FROM messages WHERE from_agent = 'relay' AND to_agent = ? AND reply_to = ?`, bearer, ask).Scan(&body); err != nil {
		t.Fatalf("sanction to %s: %v", bearer, err)
	}
	return body
}

// TestSameTaskMessageFulfilsAnswerAtBreach (AC1): asker asks rec on task T;
// rec writes asker a new message on T with no reply_to and no id cite; the
// breach sweep fulfils answer.reply with match=task_id and escalates nothing.
func TestSameTaskMessageFulfilsAnswerAtBreach(t *testing.T) {
	f := escalationFixture(t)
	f.seedTask(t, "task-t")
	ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "decide", "task_id": "task-t", "content": "A or B?"})
	reply := f.send(t, map[string]any{"as": "rec", "to": "asker", "task_id": "task-t", "subject": "ruling", "content": "B"})

	f.sweep(time.Now().UTC().Add(61 * time.Minute))
	f.assertNoEscalation(t, ask)
	if ev := f.breachEvidence(t, ask); ev["reply"] != reply || ev["match"] != "task_id" {
		t.Fatalf("evidence = %v, want reply %s match task_id", ev, reply)
	}
}

// TestSameTaskMessageWrongPartyOrTaskStillEscalates (AC2): with no message
// from rec to asker on T, the rung escalates as today: nothing sent, a
// message on another task, rec writing a third agent on T, or the asker
// writing rec on T.
func TestSameTaskMessageWrongPartyOrTaskStillEscalates(t *testing.T) {
	cases := map[string]func(f *answerFixture){
		"no message": func(f *answerFixture) {},
		"other task": func(f *answerFixture) {
			f.send(t, map[string]any{"as": "rec", "to": "asker", "task_id": "task-u", "content": "other work"})
		},
		"bearer to third agent": func(f *answerFixture) {
			f.send(t, map[string]any{"as": "rec", "to": "other", "task_id": "task-t", "content": "B"})
		},
		"asker to bearer": func(f *answerFixture) {
			f.send(t, map[string]any{"as": "asker", "to": "rec", "task_id": "task-t", "content": "ping"})
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			f := escalationFixture(t)
			if res, _ := f.h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "other"})); res.IsError {
				t.Fatalf("register other: %s", expectError(t, res))
			}
			f.seedTask(t, "task-t")
			f.seedTask(t, "task-u")
			ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask", "task_id": "task-t", "content": "drop column x?"})
			write(f)
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

// TestAnswerSanctionQuotesBearerLastMessage (AC3): the escalation notice
// names rec's last message to asker after the ask (id and subject), and says
// nothing of the kind when rec wrote asker nothing since the ask.
func TestAnswerSanctionQuotesBearerLastMessage(t *testing.T) {
	f := escalationFixture(t)
	f.send(t, map[string]any{"as": "rec", "to": "asker", "subject": "before the ask", "content": "old"})
	ask := f.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask", "content": "drop column x?"})
	f.send(t, map[string]any{"as": "rec", "to": "asker", "subject": "first after", "content": "looking"})
	last := f.send(t, map[string]any{"as": "rec", "to": "asker", "subject": "status update", "content": "still on the migration"})

	f.sweep(time.Now().UTC().Add(61 * time.Minute))
	body := f.sanctionTo(t, "mgr", ask)
	if !strings.Contains(body, last[:8]) || !strings.Contains(body, "status update") {
		t.Fatalf("sanction %q does not quote rec's last message %s 'status update'", body, last[:8])
	}
	if strings.Contains(body, "before the ask") || strings.Contains(body, "first after") {
		t.Fatalf("sanction %q quotes a message other than rec's last one after the ask", body)
	}

	g := escalationFixture(t)
	ask2 := g.send(t, map[string]any{"as": "asker", "to": "rec", "action_required": "ask", "content": "drop column x?"})
	g.sweep(time.Now().UTC().Add(61 * time.Minute))
	if body := g.sanctionTo(t, "mgr", ask2); strings.Contains(body, "last wrote") {
		t.Fatalf("sanction %q mentions a last message although rec wrote nothing", body)
	}
}
