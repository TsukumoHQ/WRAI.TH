package db

import "testing"

// inboxHas reports whether agentName's inbox surfaces the given message id.
func inboxHas(t *testing.T, d *DB, project, agentName, msgID string) bool {
	t.Helper()
	msgs, err := d.GetInbox(project, agentName, false, 100)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	for _, m := range msgs {
		if m.ID == msgID {
			return true
		}
	}
	return false
}

// Policy (cto T6): P0 never auto-expires regardless of caller ttl; P1 gets a long
// default when unspecified; P2/P3 keep the 4h default. normalizeTTL is the insert-side
// enforcement point.
func TestNormalizeTTLPolicy(t *testing.T) {
	cases := []struct {
		priority string
		in       int
		want     int
	}{
		{"P0", 3600, 0},    // caller ttl IGNORED for P0
		{"P0", 0, 0},       // already never
		{"P0", -1, 0},      // unspecified
		{"P1", -1, 604800}, // long default when unspecified
		{"P1", 120, 120},   // explicit ttl respected
		{"P2", -1, 14400},  // 4h default unchanged
		{"P3", -1, 14400},  // 4h default unchanged
		{"P2", 90, 90},     // explicit respected
	}
	for _, c := range cases {
		if got := normalizeTTL(c.priority, c.in); got != c.want {
			t.Errorf("normalizeTTL(%s, %d) = %d, want %d", c.priority, c.in, got, c.want)
		}
	}
}

// A P0 inserted with a nonzero caller ttl is stored as never-expire (ttl=0),
// survives an expiry sweep, and stays surfaced in the inbox.
func TestP0InsertedTTLForcedNeverExpires(t *testing.T) {
	d := testDB(t)
	m, err := d.InsertMessageWithDeliveries("p1", "s", "bot-a", "notification", "urgent", "c", "{}", "P0", 3600, nil, nil, []string{"bot-a"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if m.TTLSeconds != 0 {
		t.Errorf("P0 ttl must be forced to 0, got %d", m.TTLSeconds)
	}
	// Sweep must not expire it.
	if _, err := d.ExpireMessages(); err != nil {
		t.Fatalf("expire: %v", err)
	}
	var exp *string
	if err := d.conn.QueryRow("SELECT expired_at FROM messages WHERE id = ?", m.ID).Scan(&exp); err != nil {
		t.Fatalf("read expired_at: %v", err)
	}
	if exp != nil {
		t.Errorf("P0 must not be stamped expired_at, got %v", *exp)
	}
	if !inboxHas(t, d, "p1", "bot-a", m.ID) {
		t.Error("P0 must remain surfaced in inbox after sweep")
	}
}

// Belt vs suspenders: even a LEGACY P0 whose ttl_seconds>0 (a row that predates
// the policy, or a direct-insert path bypassing normalizeTTL) must survive the
// sweep (priority != 'P0' guard) AND stay surfaced (inbox predicate P0-exempt),
// even after its TTL window has elapsed.
func TestLegacyP0WithTTLStillNeverExpires(t *testing.T) {
	d := testDB(t)
	m, err := d.InsertMessageWithDeliveries("p1", "s", "bot-a", "notification", "urgent", "c", "{}", "P0", 0, nil, nil, []string{"bot-a"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Simulate a legacy P0: ttl>0 AND created long enough ago that the TTL window
	// has already elapsed (so the datetime predicate branch is false for it).
	if _, err := d.conn.Exec(
		"UPDATE messages SET ttl_seconds = 60, created_at = datetime('now','-1 hour') WHERE id = ?", m.ID,
	); err != nil {
		t.Fatalf("mutate legacy ttl: %v", err)
	}
	// Sweep: the priority guard must skip it.
	if _, err := d.ExpireMessages(); err != nil {
		t.Fatalf("expire: %v", err)
	}
	var exp *string
	if err := d.conn.QueryRow("SELECT expired_at FROM messages WHERE id = ?", m.ID).Scan(&exp); err != nil {
		t.Fatalf("read expired_at: %v", err)
	}
	if exp != nil {
		t.Errorf("legacy P0 must not be stamped expired_at, got %v", *exp)
	}
	// Inbox predicate must still surface it despite the elapsed TTL window.
	if !inboxHas(t, d, "p1", "bot-a", m.ID) {
		t.Error("legacy P0 with elapsed ttl must still surface (inbox predicate P0-exempt)")
	}
}

// A non-P0 message with an elapsed TTL still expires — the policy must not
// accidentally keep everything alive.
func TestNonP0StillExpires(t *testing.T) {
	d := testDB(t)
	m, err := d.InsertMessageWithDeliveries("p1", "s", "bot-a", "notification", "stale", "c", "{}", "P2", 0, nil, nil, []string{"bot-a"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := d.conn.Exec(
		"UPDATE messages SET ttl_seconds = 60, created_at = datetime('now','-1 hour') WHERE id = ?", m.ID,
	); err != nil {
		t.Fatalf("mutate ttl: %v", err)
	}
	n, err := d.ExpireMessages()
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Errorf("expired P2 should be swept, got %d", n)
	}
	if inboxHas(t, d, "p1", "bot-a", m.ID) {
		t.Error("expired P2 must not surface in inbox")
	}
}
