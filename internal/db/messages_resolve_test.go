package db

import (
	"testing"
	"time"
)

// DEC-wraith-tombstones-1 T2 (task 93b6f1cf): message refs resolve through
// tombstones, and a reply to a purged parent still inherits its tag and trace.

// purgeNow soft-expires id long ago and runs the purge, leaving a tombstone.
func purgeNow(t *testing.T, d *DB, id string) {
	t.Helper()
	expireLongAgo(t, d, id)
	if _, err := d.PurgeExpiredMessages(time.Hour); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones WHERE id = ?`, id); n != 1 {
		t.Fatalf("fixture: %s not tombstoned", id)
	}
}

func TestResolve(t *testing.T) {
	t.Run("Live", func(t *testing.T) {
		d := retentionTestDB(t)
		m, err := d.InsertMessage("p1", "a", "b", "notification", "s", "c", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if got := d.ResolveMessageRef("p1", m.ID); got != RefLive {
			t.Fatalf("got %s, want live", got)
		}
	})

	t.Run("Tombstoned", func(t *testing.T) {
		d := retentionTestDB(t)
		m, err := d.InsertMessage("p1", "a", "b", "notification", "s", "c", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		purgeNow(t, d, m.ID)
		if got := d.ResolveMessageRef("p1", m.ID); got != RefTombstoned {
			t.Fatalf("got %s, want tombstoned", got)
		}
	})

	t.Run("Unknown", func(t *testing.T) {
		d := retentionTestDB(t)
		if got := d.ResolveMessageRef("p1", "11111111-2222-3333-4444-555555555555"); got != RefUnknown {
			t.Fatalf("got %s, want unknown", got)
		}
	})

	t.Run("CrossProjectIsUnknown", func(t *testing.T) {
		d := retentionTestDB(t)
		live, err := d.InsertMessage("p2", "a", "b", "notification", "s", "c", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		gone, err := d.InsertMessage("p2", "a", "b", "notification", "s", "c", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		purgeNow(t, d, gone.ID)
		for _, id := range []string{live.ID, gone.ID} {
			if got := d.ResolveMessageRef("p1", id); got != RefUnknown {
				t.Fatalf("%s from p2 resolved in p1 as %s, want unknown", id, got)
			}
		}
	})

	t.Run("ReplyToTombstonedParentInheritsActionTag", func(t *testing.T) {
		d := retentionTestDB(t)
		q, err := d.InsertMessage("p1", "a", "b", "question", "s", "?", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert question: %v", err)
		}
		purgeNow(t, d, q.ID)
		r, _, err := d.InsertMessageWithDeliveries("p1", "b", "a", "response", "re", "!", "{}", "P2", 3600, &q.ID, nil, []string{"a"}, "")
		if err != nil {
			t.Fatalf("insert reply: %v", err)
		}
		if r.ActionRequired == nil || *r.ActionRequired != "ask" {
			t.Fatalf("reply action_required = %v, want ask (inherited from the tombstone, not the do fallback)", r.ActionRequired)
		}
	})

	t.Run("ReplyToTombstonedParentInheritsTraceID", func(t *testing.T) {
		d := retentionTestDB(t)
		q, err := d.InsertMessage("p1", "a", "b", "question", "s", "?", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert question: %v", err)
		}
		if _, err := d.conn.Exec(`UPDATE messages SET trace_id = 'trace-abc' WHERE id = ?`, q.ID); err != nil {
			t.Fatalf("set trace: %v", err)
		}
		purgeNow(t, d, q.ID)
		r, _, err := d.InsertMessageWithDeliveries("p1", "b", "a", "response", "re", "!", "{}", "P2", 3600, &q.ID, nil, []string{"a"}, "")
		if err != nil {
			t.Fatalf("insert reply: %v", err)
		}
		if r.TraceID == nil || *r.TraceID != "trace-abc" {
			t.Fatalf("reply trace_id = %v, want trace-abc from the tombstone", r.TraceID)
		}
	})
}
