package db

import (
	"strings"
	"testing"
)

// getInboxLegacy runs only when the deliveries table is empty (legacy DBs).
// InsertMessage (no deliveries) keeps it empty, so GetInbox routes here. The
// UNION-ALL rewrite must return the SAME set the old OR predicate did: direct
// DMs, broadcasts from others, and conversation messages from others — excluding
// self-sent broadcasts, DMs to others, own conversation messages, and messages in
// conversations the agent has left / never joined.
func TestGetInboxLegacy_UnionCorrectness(t *testing.T) {
	d := testDB(t)
	if d.HasDeliveries() {
		t.Fatal("precondition: no deliveries expected")
	}

	mk := func(from, to string, conv *string) string {
		msg, err := d.InsertMessage("lg", from, to, "notification", "s", "c", "{}", "P2", 3600, nil, conv)
		if err != nil {
			t.Fatalf("insert %s→%s: %v", from, to, err)
		}
		return msg.ID
	}

	dm := mk("x", "me", nil) // included: direct DM to me
	bc := mk("x", "*", nil)  // included: broadcast from another
	mk("me", "*", nil)       // excluded: my own broadcast
	mk("x", "other", nil)    // excluded: DM to someone else

	conv, err := d.CreateConversation("lg", "c1", "me", []string{"me", "x"})
	if err != nil {
		t.Fatalf("create conv: %v", err)
	}
	convFromX := mk("x", "", &conv.ID) // included: conv message from another member
	mk("me", "", &conv.ID)             // excluded: my own conv message

	other, err := d.CreateConversation("lg", "c2", "x", []string{"x", "y"})
	if err != nil {
		t.Fatalf("create conv2: %v", err)
	}
	mk("x", "", &other.ID) // excluded: conversation I'm not a member of

	msgs, err := d.GetInbox("lg", "me", false, 50)
	if err != nil {
		t.Fatalf("get inbox: %v", err)
	}
	got := map[string]bool{}
	for _, m := range msgs {
		got[m.ID] = true
	}
	want := []string{dm, bc, convFromX}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), got)
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("missing expected message %s", id)
		}
	}

	// ExcludeBroadcasts drops the broadcast branch.
	noBc, err := d.GetInbox("lg", "me", false, 50, InboxFilter{ExcludeBroadcasts: true})
	if err != nil {
		t.Fatalf("get inbox no-bc: %v", err)
	}
	for _, m := range noBc {
		if m.ID == bc {
			t.Errorf("ExcludeBroadcasts still returned the broadcast")
		}
	}
	if len(noBc) != 2 {
		t.Errorf("ExcludeBroadcasts: got %d, want 2 (dm + conv)", len(noBc))
	}

	// unreadOnly: the NOT EXISTS over the union must drop a read message.
	if _, err := d.MarkRead([]string{dm}, "me", "lg"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	unread, err := d.GetInbox("lg", "me", true, 50)
	if err != nil {
		t.Fatalf("get inbox unread: %v", err)
	}
	for _, m := range unread {
		if m.ID == dm {
			t.Errorf("unreadOnly still returned the read DM")
		}
	}
	if len(unread) != 2 {
		t.Errorf("unreadOnly: got %d, want 2 (bc + conv)", len(unread))
	}
}

// The legacy direct-DM selection must be index-driven (SEARCH via
// idx_messages_project_to), NOT a whole-project SCAN — the relay-perf R1 target.
func TestGetInboxLegacy_IndexedNotScan(t *testing.T) {
	d := testDB(t)
	dmBranch := `SELECT m.id FROM messages m
		WHERE m.project = ? AND m.conversation_id IS NULL AND m.to_agent = ?
		ORDER BY m.created_at DESC LIMIT ?`
	rows, err := d.ro().Query("EXPLAIN QUERY PLAN "+dmBranch, "lg", "me", 50)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		_ = rows.Scan(&id, &parent, &notused, &detail)
		plan.WriteString(detail + "\n")
	}
	p := plan.String()
	if !strings.Contains(p, "idx_messages_project_to") {
		t.Fatalf("DM branch not using idx_messages_project_to; plan:\n%s", p)
	}
	if strings.Contains(p, "SCAN m\n") || strings.Contains(p, "SCAN messages") {
		t.Fatalf("DM branch is a full SCAN; plan:\n%s", p)
	}
}
