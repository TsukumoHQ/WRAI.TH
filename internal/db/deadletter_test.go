package db

import (
	"testing"
	"time"
)

// expireMessageNow forces a message's TTL to be considered elapsed by stamping a
// past expired_at directly (the sweep that normally does this is out of scope).
func expireMessageNow(t *testing.T, d *DB, msgID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000000Z")
	if _, err := d.conn.Exec("UPDATE messages SET expired_at = ? WHERE id = ?", past, msgID); err != nil {
		t.Fatalf("stamp expired_at: %v", err)
	}
}

// T6: an unread P0 that TTL-expires is journaled to the durable deadletter with
// full metadata, and only UNREAD deliveries are journaled (an acked one is not).
func TestDeadletterJournalsExpiredUnread(t *testing.T) {
	d := testDB(t)
	m, err := d.InsertMessageWithDeliveries("p1", "sender", "bot-a", "notification", "urgent", "body", "{}", "P0", 0, nil, nil, []string{"bot-a", "bot-b"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// bot-b acks (has seen it) — must NOT be dead-lettered. bot-a stays unread.
	if err := d.AcknowledgeDeliveryByMessage(m.ID, "bot-b", "p1"); err != nil {
		t.Fatalf("ack bot-b: %v", err)
	}
	expireMessageNow(t, d, m.ID)

	n, err := d.ExpireDeliveries()
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Errorf("only bot-a's unread delivery should expire, got %d", n)
	}

	dl, err := d.Deadletter("p1", "bot-a", 50)
	if err != nil {
		t.Fatalf("deadletter: %v", err)
	}
	if len(dl) != 1 {
		t.Fatalf("bot-a should have 1 deadletter row, got %d", len(dl))
	}
	r := dl[0]
	if r.MessageID != m.ID || r.From != "sender" || r.Priority != "P0" || r.Subject != "urgent" {
		t.Errorf("deadletter metadata wrong: %+v", r)
	}
	if r.ExpiredAt == "" {
		t.Error("deadletter row missing expired_at")
	}
	// bot-b acked, so it must not appear in the deadletter.
	if b, _ := d.Deadletter("p1", "bot-b", 50); len(b) != 0 {
		t.Errorf("acked recipient must not be dead-lettered, got %d", len(b))
	}
}

// T6: re-running the expiry sweep does not duplicate deadletter rows (UNIQUE
// message_id,to_agent + INSERT OR IGNORE).
func TestDeadletterIdempotent(t *testing.T) {
	d := testDB(t)
	m, err := d.InsertMessageWithDeliveries("p1", "s", "bot-a", "notification", "s", "c", "{}", "P1", 0, nil, nil, []string{"bot-a"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	expireMessageNow(t, d, m.ID)
	if _, err := d.ExpireDeliveries(); err != nil {
		t.Fatalf("expire 1: %v", err)
	}
	if _, err := d.ExpireDeliveries(); err != nil {
		t.Fatalf("expire 2: %v", err)
	}
	dl, _ := d.Deadletter("p1", "bot-a", 50)
	if len(dl) != 1 {
		t.Errorf("re-run must not duplicate; want 1 row, got %d", len(dl))
	}
}

// T6: the deadletter record SURVIVES the retention GC that hard-deletes the
// expired message + its deliveries — the whole point (a P0 must not vanish).
func TestDeadletterSurvivesPurge(t *testing.T) {
	d := testDB(t)
	m, err := d.InsertMessageWithDeliveries("p1", "s", "bot-a", "notification", "keeps", "c", "{}", "P0", 0, nil, nil, []string{"bot-a"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	expireMessageNow(t, d, m.ID)
	if _, err := d.ExpireDeliveries(); err != nil {
		t.Fatalf("expire: %v", err)
	}
	// GC hard-deletes the expired message + deliveries (grace 0 = purge now).
	if _, err := d.PurgeExpiredMessages(0); err != nil {
		t.Fatalf("purge: %v", err)
	}
	dl, err := d.Deadletter("p1", "bot-a", 50)
	if err != nil {
		t.Fatalf("deadletter post-purge: %v", err)
	}
	if len(dl) != 1 || dl[0].Priority != "P0" || dl[0].Subject != "keeps" {
		t.Errorf("deadletter must survive GC with metadata intact, got %+v", dl)
	}
}
