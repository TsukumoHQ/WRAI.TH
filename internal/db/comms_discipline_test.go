package db

import "testing"

// derivation: an omitted action_required is computed from (type, reply_to).
func TestActionRequiredDerivation(t *testing.T) {
	d := testDB(t)

	cases := []struct {
		msgType string
		want    string
	}{
		{"task", "do"},
		{"question", "ask"},
		{"user_question", "ask"},
		{"notification", "none"},
		{"status", "none"},
		{"ack", "none"},
		{"fyi", "none"},
		{"message", "none"},
	}
	for _, c := range cases {
		msg, _, err := d.InsertMessageWithDeliveries("p", "a", "b", c.msgType, "s", "body", "{}", "P2", 3600, nil, nil, []string{"b"}, "")
		if err != nil {
			t.Fatalf("insert %s: %v", c.msgType, err)
		}
		if msg.ActionRequired == nil || *msg.ActionRequired != c.want {
			t.Fatalf("type %s derived %v, want %s", c.msgType, msg.ActionRequired, c.want)
		}
	}
}

// a thread-opening response (reply_to nil) is none; a reply to a question
// inherits the question's tag (ask) so the asker is woken for the answer.
func TestResponseInheritance(t *testing.T) {
	d := testDB(t)

	// Opening response, no parent → none.
	open, _, _ := d.InsertMessageWithDeliveries("p", "a", "b", "response", "s", "body", "{}", "P2", 3600, nil, nil, []string{"b"}, "")
	if open.ActionRequired == nil || *open.ActionRequired != "none" {
		t.Fatalf("thread-opening response should be none, got %v", open.ActionRequired)
	}

	// A question, then a response replying to it → inherits ask.
	q, _, _ := d.InsertMessageWithDeliveries("p", "b", "a", "question", "q", "body", "{}", "P2", 3600, nil, nil, []string{"a"}, "")
	reply, _, _ := d.InsertMessageWithDeliveries("p", "a", "b", "response", "r", "body", "{}", "P2", 3600, &q.ID, nil, []string{"b"}, "")
	if reply.ActionRequired == nil || *reply.ActionRequired != "ask" {
		t.Fatalf("reply to a question should inherit ask, got %v", reply.ActionRequired)
	}
}

// an explicit tag overrides derivation.
func TestActionRequiredExplicitOverride(t *testing.T) {
	d := testDB(t)
	// A notification (would derive none) sent with explicit do.
	msg, _, _ := d.InsertMessageWithDeliveries("p", "a", "b", "notification", "s", "body", "{}", "P2", 3600, nil, nil, []string{"b"}, "do")
	if msg.ActionRequired == nil || *msg.ActionRequired != "do" {
		t.Fatalf("explicit tag should override, got %v", msg.ActionRequired)
	}
}

// The wake predicate: none is excluded from the wake count; ask/do/task/question
// and P0 are counted; the guard makes none unable to suppress a question or P0.
func TestWakeCountHonorsActionRequired(t *testing.T) {
	d := testDB(t)

	count := func(agent string) int {
		n, err := d.UnreadCountForAgent("p", agent)
		if err != nil {
			t.Fatalf("unread count %s: %v", agent, err)
		}
		return n
	}

	// notification (derives none) → NOT counted.
	_, _, _ = d.InsertMessageWithDeliveries("p", "a", "n1", "notification", "s", "body", "{}", "P2", 3600, nil, nil, []string{"n1"}, "")
	if got := count("n1"); got != 0 {
		t.Fatalf("none-tagged notification must not wake, count=%d", got)
	}

	// question → counted.
	_, _, _ = d.InsertMessageWithDeliveries("p", "a", "n2", "question", "s", "body", "{}", "P2", 3600, nil, nil, []string{"n2"}, "")
	if got := count("n2"); got != 1 {
		t.Fatalf("question must wake, count=%d", got)
	}

	// task → counted.
	_, _, _ = d.InsertMessageWithDeliveries("p", "a", "n3", "task", "s", "body", "{}", "P2", 3600, nil, nil, []string{"n3"}, "")
	if got := count("n3"); got != 1 {
		t.Fatalf("task must wake, count=%d", got)
	}

	// notification with explicit do → counted (opt-in wake).
	_, _, _ = d.InsertMessageWithDeliveries("p", "a", "n4", "notification", "s", "body", "{}", "P2", 3600, nil, nil, []string{"n4"}, "do")
	if got := count("n4"); got != 1 {
		t.Fatalf("do-tagged notification must wake, count=%d", got)
	}

	// GUARD: a P0 message explicitly tagged none STILL wakes (P0 guard clause).
	_, _, _ = d.InsertMessageWithDeliveries("p", "a", "n5", "notification", "s", "body", "{}", "P0", 0, nil, nil, []string{"n5"}, "none")
	if got := count("n5"); got != 1 {
		t.Fatalf("P0 must wake even when tagged none, count=%d", got)
	}

	// GUARD: a question explicitly tagged none STILL wakes (type guard clause).
	_, _, _ = d.InsertMessageWithDeliveries("p", "a", "n6", "question", "s", "body", "{}", "P2", 3600, nil, nil, []string{"n6"}, "none")
	if got := count("n6"); got != 1 {
		t.Fatalf("question must wake even when tagged none, count=%d", got)
	}
}

// backward-compat: a legacy row with NULL action_required still wakes
// (COALESCE(...,'do')) — an old client / pre-migration message never goes silent.
func TestWakeCountLegacyNullWakes(t *testing.T) {
	d := testDB(t)
	msg, _, _ := d.InsertMessageWithDeliveries("p", "a", "leg", "notification", "s", "body", "{}", "P2", 3600, nil, nil, []string{"leg"}, "")
	// Simulate a legacy row: clear the tag to NULL.
	if _, err := d.conn.Exec("UPDATE messages SET action_required = NULL WHERE id = ?", msg.ID); err != nil {
		t.Fatalf("null-out: %v", err)
	}
	n, err := d.UnreadCountForAgent("p", "leg")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("legacy NULL tag must wake (no fleet break), count=%d", n)
	}
}
