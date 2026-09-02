package db

import (
	"path/filepath"
	"testing"
)

// WRAITH-2: the wake signal (UnreadCountForAgent) counts only NEW ('queued')
// deliveries. Once a delivery is surfaced (shown by get_inbox), it must stop
// waking the agent — but stay visible in the inbox's unread view until acked.
func TestUnreadCountExcludesSurfaced(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.RegisterAgent("p", "bob", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := d.InsertMessageWithDeliveries("p", "alice", "bob", "note", "hi", "body", "", "P2", 0, nil, nil, []string{"bob"}, ""); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// New delivery is 'queued' → wakes.
	if n, _ := d.UnreadCountForAgent("p", "bob"); n != 1 {
		t.Fatalf("queued delivery: want wake-count 1, got %d", n)
	}

	// get_inbox surfaces the delivery (marks queued → surfaced).
	msgs, err := d.GetInbox("p", "bob", true, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("get_inbox: got %d msgs, err %v", len(msgs), err)
	}

	// Surfaced → no longer wakes.
	if n, _ := d.UnreadCountForAgent("p", "bob"); n != 0 {
		t.Errorf("surfaced delivery must not wake: want 0, got %d", n)
	}
	// ...but is still in the unread VIEW until an explicit ack (non-destructive peek).
	stillMsgs, _ := d.GetInbox("p", "bob", true, 10)
	if len(stillMsgs) != 1 {
		t.Errorf("surfaced message must stay in unread view, got %d", len(stillMsgs))
	}
}

// WRAITH-2: mark_read(conversation_id) must acknowledge the conversation's
// deliveries, not just write conversation_reads — otherwise they resurface.
func TestAcknowledgeConversationDeliveries(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.RegisterAgent("p", "bob", "dev", "", nil, nil, false, nil, "[]", 0, RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	cid := "conv-1"
	if _, _, err := d.InsertMessageWithDeliveries("p", "alice", "", "note", "s", "c", "", "P2", 0, nil, &cid, []string{"bob"}, ""); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n, _ := d.UnreadCountForAgent("p", "bob"); n != 1 {
		t.Fatalf("pre-ack wake-count: want 1, got %d", n)
	}

	if err := d.AcknowledgeConversationDeliveries(cid, "bob", "p"); err != nil {
		t.Fatalf("ack conversation: %v", err)
	}

	// Acknowledged → gone from the wake count AND the unread view.
	if n, _ := d.UnreadCountForAgent("p", "bob"); n != 0 {
		t.Errorf("acked conversation delivery must not wake: got %d", n)
	}
	if msgs, _ := d.GetInbox("p", "bob", true, 10); len(msgs) != 0 {
		t.Errorf("acked conversation message must leave unread view, got %d", len(msgs))
	}
}
