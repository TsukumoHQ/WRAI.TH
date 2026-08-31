package db

import "testing"

// Task ac328091: a caller-supplied idempotency_key lets a genuine client retry
// (same logical send, resubmitted after a timeout) short-circuit to the
// original message instead of creating a duplicate — while legitimate
// identical-content resends WITHOUT a key (heartbeats, repeated status) stay
// completely unaffected.

func TestInsertMessageWithDeliveries_SameIdempotencyKeyReturnsSameMessage(t *testing.T) {
	d := testDB(t)

	m1, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "s", "body", "{}", "P2", 0, nil, nil, []string{"bot-a", "bot-b"}, "", "retry-key-1")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	m2, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "s", "body", "{}", "P2", 0, nil, nil, []string{"bot-a", "bot-b"}, "", "retry-key-1")
	if err != nil {
		t.Fatalf("second insert (retry): %v", err)
	}
	if m1.ID != m2.ID {
		t.Fatalf("expected same message ID on retry, got %s then %s", m1.ID, m2.ID)
	}

	var msgCount int
	if err := d.conn.QueryRow("SELECT COUNT(*) FROM messages WHERE id = ?", m1.ID).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("expected exactly 1 message row, got %d", msgCount)
	}

	var deliveryCount int
	if err := d.conn.QueryRow("SELECT COUNT(*) FROM deliveries WHERE message_id = ?", m1.ID).Scan(&deliveryCount); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveryCount != 2 {
		t.Fatalf("expected exactly 2 delivery rows (one per recipient, no duplicates), got %d", deliveryCount)
	}
}

func TestInsertMessageWithDeliveries_DifferentOrNoKeyCreatesNewMessage(t *testing.T) {
	d := testDB(t)

	m1, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "s", "body", "{}", "P2", 0, nil, nil, []string{"bot-a"}, "", "key-a")
	if err != nil {
		t.Fatalf("insert with key-a: %v", err)
	}
	m2, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "s", "body", "{}", "P2", 0, nil, nil, []string{"bot-a"}, "", "key-b")
	if err != nil {
		t.Fatalf("insert with key-b: %v", err)
	}
	if m1.ID == m2.ID {
		t.Fatalf("different idempotency keys must not collide, got same ID %s", m1.ID)
	}

	m3, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "s", "body", "{}", "P2", 0, nil, nil, []string{"bot-a"}, "")
	if err != nil {
		t.Fatalf("insert with no key: %v", err)
	}
	m4, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "s", "body", "{}", "P2", 0, nil, nil, []string{"bot-a"}, "")
	if err != nil {
		t.Fatalf("second insert with no key: %v", err)
	}
	if m3.ID == m4.ID {
		t.Fatalf("no-key sends (legitimate identical-content resends) must never dedup, got same ID %s", m3.ID)
	}
}
