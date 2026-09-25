package relay

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// Answer obligations, slice A (task 044a4876): a direct or team ask/decide
// opens answer.reply on each recipient; the bearer's reply (reply_to chain,
// tombstones included) fulfils it; obligation_decline closes it.

type answerFixture struct {
	h   *Handlers
	raw *sql.DB
}

func newAnswerFixture(t *testing.T, agents ...string) *answerFixture {
	t.Helper()
	d, path := memDB(t)
	h := memHandlersAt(t, d)
	raw, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, a := range agents {
		if res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": a})); res.IsError {
			t.Fatalf("register %s: %s", a, expectError(t, res))
		}
	}
	return &answerFixture{h: h, raw: raw}
}

// send posts a message through send_message and returns its id.
func (f *answerFixture) send(t *testing.T, args map[string]any) string {
	t.Helper()
	args["project"] = "p1"
	if _, ok := args["subject"]; !ok {
		args["subject"] = "s"
	}
	if _, ok := args["content"]; !ok {
		args["content"] = "c"
	}
	res, _ := f.h.HandleSendMessage(ctx, call(args))
	if res.IsError {
		t.Fatalf("send %v: %s", args, expectError(t, res))
	}
	return parseJSON(t, res)["id"].(string)
}

// answers returns bearer -> state of the answer obligations on a message.
func (f *answerFixture) answers(t *testing.T, msgID string) map[string]string {
	t.Helper()
	rows, err := f.raw.Query(`SELECT bearer, state FROM obligations WHERE subject_kind = 'message' AND subject_id = ?`, msgID)
	if err != nil {
		t.Fatalf("answers: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var bearer, state string
		if err := rows.Scan(&bearer, &state); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, dup := out[bearer]; dup {
			t.Fatalf("two answer obligations for %s on %s", bearer, msgID)
		}
		out[bearer] = state
	}
	return out
}

func (f *answerFixture) answerID(t *testing.T, msgID, bearer string) string {
	t.Helper()
	var id string
	if err := f.raw.QueryRow(`SELECT id FROM obligations WHERE subject_kind = 'message' AND subject_id = ? AND bearer = ?`, msgID, bearer).Scan(&id); err != nil {
		t.Fatalf("answer id %s/%s: %v", msgID, bearer, err)
	}
	return id
}

// TestAnswerObligationAskFulfilledByReply (AC1): an ask to a two-member team
// opens one active answer.reply per recipient on that one message; a reply from
// recipient A fulfils A's only. A direct ask opens exactly one.
func TestAnswerObligationAskFulfilledByReply(t *testing.T) {
	f := newAnswerFixture(t, "asker", "alice", "bob")
	_, _ = f.h.HandleCreateTeam(ctx, call(map[string]any{"project": "p1", "name": "Pair", "slug": "pair"}))
	for _, m := range []string{"asker", "alice", "bob"} {
		_, _ = f.h.HandleAddTeamMember(ctx, call(map[string]any{"project": "p1", "team": "pair", "agent_name": m}))
	}

	ask := f.send(t, map[string]any{"as": "asker", "to": "team:pair", "action_required": "ask"})
	got := f.answers(t, ask)
	if len(got) != 2 || got["alice"] != db.ObligationActive || got["bob"] != db.ObligationActive {
		t.Fatalf("after ask: %v, want alice+bob active", got)
	}
	var norm, kind string
	if err := f.raw.QueryRow(`SELECT norm_id, subject_kind FROM obligations WHERE subject_id = ? AND bearer = 'alice'`, ask).Scan(&norm, &kind); err != nil ||
		norm != db.NormAnswerReply || kind != db.SubjectMessage {
		t.Fatalf("alice obligation norm=%q kind=%q (%v), want %s/%s", norm, kind, err, db.NormAnswerReply, db.SubjectMessage)
	}

	f.send(t, map[string]any{"as": "alice", "to": "asker", "reply_to": ask, "content": "yes"})
	if got := f.answers(t, ask); got["alice"] != db.ObligationFulfilled || got["bob"] != db.ObligationActive {
		t.Fatalf("after alice's reply: %v, want alice fulfilled, bob active", got)
	}

	direct := f.send(t, map[string]any{"as": "asker", "to": "bob", "action_required": "ask"})
	if got := f.answers(t, direct); len(got) != 1 || got["bob"] != db.ObligationActive {
		t.Fatalf("direct ask: %v, want bob active only", got)
	}
}

// TestAnswerObligationDecideDeclineAndTombstone (AC2): a decide opens one
// obligation that obligation_decline closes with its reason class; a second
// decide is fulfilled by a reply whose chain passes through a purged message
// (message_tombstones.reply_to). obligation_discharge re-checks the reply.
func TestAnswerObligationDecideDeclineAndTombstone(t *testing.T) {
	f := newAnswerFixture(t, "lead", "dev")

	decide := f.send(t, map[string]any{"as": "lead", "to": "dev", "action_required": "decide"})
	if got := f.answers(t, decide); len(got) != 1 || got["dev"] != db.ObligationActive {
		t.Fatalf("decide: %v, want dev active", got)
	}
	id := f.answerID(t, decide, "dev")
	if res, _ := f.h.HandleObligationDecline(ctx, call(map[string]any{"project": "p1", "as": "lead", "id": id, "reason_class": "not_mine"})); !res.IsError {
		t.Fatal("a non-bearer declined the obligation")
	}
	res, _ := f.h.HandleObligationDecline(ctx, call(map[string]any{"project": "p1", "as": "dev", "id": id, "reason_class": "not_mine"}))
	if res.IsError {
		t.Fatalf("decline: %s", expectError(t, res))
	}
	var state, reason string
	if err := f.raw.QueryRow(`SELECT state, COALESCE(decline_reason_class, '') FROM obligations WHERE id = ?`, id).Scan(&state, &reason); err != nil ||
		state != db.ObligationUnfulfilled || reason != "not_mine" {
		t.Fatalf("declined obligation state=%q reason=%q (%v), want unfulfilled/not_mine", state, reason, err)
	}

	second := f.send(t, map[string]any{"as": "lead", "to": "dev", "action_required": "decide"})
	secondID := f.answerID(t, second, "dev")
	if res, _ := f.h.HandleObligationDischarge(ctx, call(map[string]any{"project": "p1", "as": "dev", "id": secondID})); !res.IsError {
		t.Fatal("discharge succeeded with no reply")
	}
	// lead follows up on the decide, then the follow-up is purged to a tombstone.
	followUp := f.send(t, map[string]any{"as": "lead", "to": "dev", "reply_to": second, "content": "any news?"})
	if _, err := f.raw.Exec(`INSERT INTO message_tombstones (id, project, from_agent, to_agent, type, reply_to, created_at, purged_at)
		SELECT id, project, from_agent, to_agent, type, reply_to, created_at, ? FROM messages WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), followUp); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if _, err := f.raw.Exec(`DELETE FROM messages WHERE id = ?`, followUp); err != nil {
		t.Fatalf("purge: %v", err)
	}
	f.send(t, map[string]any{"as": "dev", "to": "lead", "reply_to": followUp, "content": "go with B"})
	if got := f.answers(t, second); got["dev"] != db.ObligationFulfilled {
		t.Fatalf("reply through tombstone: %v, want dev fulfilled", got)
	}
}

// ackRows renders the task obligations (norm, subject, bearer, state, depth,
// closure) for a byte comparison across fixtures.
func ackRows(t *testing.T, raw *sql.DB) string {
	t.Helper()
	rows, err := raw.Query(`SELECT norm_id, subject_id, bearer_kind, bearer, state, escalation_depth,
			COALESCE(decline_reason_class, ''), COALESCE(parent_obligation_id, '')
		FROM obligations WHERE subject_kind = 'task' ORDER BY norm_id, subject_id`)
	if err != nil {
		t.Fatalf("ack rows: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var norm, subject, kind, bearer, state, decline, parent string
		var depth int
		if err := rows.Scan(&norm, &subject, &kind, &bearer, &state, &depth, &decline, &parent); err != nil {
			t.Fatalf("scan ack: %v", err)
		}
		b.WriteString(strings.Join([]string{norm, subject, kind, bearer, state, decline, parent}, "|"))
		b.WriteString("|" + string(rune('0'+depth)) + "\n")
	}
	return b.String()
}

// TestAnswerObligationNoneForNonAskAndBroadcast (AC3): none/ack/fyi-tagged
// messages and a broadcast ask open nothing, and the ACK obligations of a task
// are identical with and without answer traffic.
func TestAnswerObligationNoneForNonAskAndBroadcast(t *testing.T) {
	run := func(withMessages bool) string {
		f := newAnswerFixture(t, "a", "b", "c")
		if _, err := f.raw.Exec(`INSERT INTO tasks (id, profile_slug, dispatched_by, title, status, project, dispatched_at, labels, blocked_periods, goal, acceptance_criteria, dod)
			VALUES ('t1', 'dev', 'cto', 'title', 'pending', 'p1', ?, '[]', '[]', '', '[]', '')`, ago(20*time.Minute)); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		if withMessages {
			var ids []string
			for _, tag := range []string{"none", "do"} {
				ids = append(ids, f.send(t, map[string]any{"as": "a", "to": "b", "action_required": tag}))
			}
			for _, typ := range []string{"ack", "fyi"} {
				ids = append(ids, f.send(t, map[string]any{"as": "a", "to": "b", "type": typ}))
			}
			ids = append(ids, f.send(t, map[string]any{"as": "a", "to": "*", "action_required": "ask"}))
			for _, id := range ids {
				if got := f.answers(t, id); len(got) != 0 {
					t.Fatalf("message %s opened %v, want none", id, got)
				}
			}
			var n int
			if err := f.raw.QueryRow(`SELECT COUNT(*) FROM obligations WHERE subject_kind = 'message'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("answer obligations = %d (%v), want 0", n, err)
			}
		}
		evaluateObligations(f.h.db, f.h.registry, time.Now().UTC())
		return ackRows(t, f.raw)
	}
	without, with := run(false), run(true)
	if without == "" {
		t.Fatal("the ACK sweep opened nothing: the comparison would be vacuous")
	}
	if with != without {
		t.Fatalf("ACK obligations differ with answer traffic:\nwithout %q\nwith    %q", without, with)
	}
}
