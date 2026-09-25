package db

import (
	"fmt"
	"testing"
	"time"
)

// DEC-wraith-tombstones-1 (task f53b2160): every message the TTL purge
// hard-deletes leaves one content-free message_tombstones row, written in the
// purge transaction before the delete.

// expireLongAgo soft-expires a message 10 days ago, past any purge grace used here.
func expireLongAgo(t *testing.T, d *DB, id string) {
	t.Helper()
	ts := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(`UPDATE messages SET expired_at = ? WHERE id = ?`, ts, id); err != nil {
		t.Fatalf("expire %s: %v", id, err)
	}
}

func TestTombstone(t *testing.T) {
	t.Run("PurgeWritesOnePerPurgedRow", func(t *testing.T) {
		d := retentionTestDB(t)
		task, err := d.DispatchTask("p1", "lead", "cto", "t", "", "P1", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		parent, err := d.InsertMessage("p1", "cto", "bot", "question", "s", "body", fmt.Sprintf(`{"task_id":%q}`, task.ID), "P1", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert parent: %v", err)
		}
		reply, err := d.InsertMessage("p1", "bot", "cto", "response", "re", "answer", "{}", "P2", 3600, &parent.ID, nil)
		if err != nil {
			t.Fatalf("insert reply: %v", err)
		}
		kept, err := d.InsertMessage("p1", "bot", "cto", "notification", "s", "still live", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert kept: %v", err)
		}
		expireLongAgo(t, d, parent.ID)
		expireLongAgo(t, d, reply.ID)

		type row struct{ project, from, to, typ, replyTo, taskID, traceID, action, priority, created string }
		read := func(table, id string) row {
			var r row
			if err := d.conn.QueryRow(`SELECT project, from_agent, to_agent, type, COALESCE(reply_to,''), COALESCE(task_id,''),
				COALESCE(trace_id,''), COALESCE(action_required,''), COALESCE(priority,''), created_at FROM `+table+` WHERE id = ?`, id).
				Scan(&r.project, &r.from, &r.to, &r.typ, &r.replyTo, &r.taskID, &r.traceID, &r.action, &r.priority, &r.created); err != nil {
				t.Fatalf("read %s %s: %v", table, id, err)
			}
			return r
		}
		want := map[string]row{parent.ID: read("messages", parent.ID), reply.ID: read("messages", reply.ID)}

		purged, tombstoned, err := d.PurgeExpiredMessagesWithTombstones(7 * 24 * time.Hour)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if purged != 2 || tombstoned != 2 {
			t.Fatalf("purged=%d tombstoned=%d, want 2/2", purged, tombstoned)
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones`); n != 2 {
			t.Fatalf("%d tombstones, want 2", n)
		}
		for id, w := range want {
			if got := read("message_tombstones", id); got != w {
				t.Fatalf("tombstone %s = %+v, want source row %+v", id, got, w)
			}
		}
		if want[parent.ID].taskID != task.ID || want[reply.ID].replyTo != parent.ID {
			t.Fatalf("fixture lost its links: %+v", want)
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones WHERE id = ?`, kept.ID); n != 0 {
			t.Fatalf("live message was tombstoned")
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones WHERE purged_at = ''`); n != 0 {
			t.Fatalf("purged_at not set")
		}
	})

	t.Run("NoContentColumns", func(t *testing.T) {
		d := retentionTestDB(t)
		rows, err := d.conn.Query(`SELECT name FROM pragma_table_info('message_tombstones')`)
		if err != nil {
			t.Fatalf("table_info: %v", err)
		}
		defer func() { _ = rows.Close() }()
		cols := 0
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan: %v", err)
			}
			cols++
			switch name {
			case "subject", "content", "metadata":
				t.Fatalf("message_tombstones must not carry content column %q", name)
			}
		}
		if cols == 0 {
			t.Fatalf("message_tombstones table missing")
		}
	})

	t.Run("IdempotentRePurge", func(t *testing.T) {
		d := retentionTestDB(t)
		m, err := d.InsertMessage("p1", "a", "b", "notification", "s", "c", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		expireLongAgo(t, d, m.ID)
		if _, _, err := d.PurgeExpiredMessagesWithTombstones(0); err != nil {
			t.Fatalf("first purge: %v", err)
		}
		purged, tombstoned, err := d.PurgeExpiredMessagesWithTombstones(0)
		if err != nil {
			t.Fatalf("second purge: %v", err)
		}
		if purged != 0 || tombstoned != 0 {
			t.Fatalf("second purge purged=%d tombstoned=%d, want 0/0", purged, tombstoned)
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones`); n != 1 {
			t.Fatalf("%d tombstones after re-purge, want 1", n)
		}
	})

	t.Run("TxAtomic", func(t *testing.T) {
		d := retentionTestDB(t)
		m, _, err := d.InsertMessageWithDeliveries("p1", "a", "b", "notification", "s", "c", "{}", "P2", 3600, nil, nil, []string{"b"}, "")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		expireLongAgo(t, d, m.ID)
		if _, err := d.conn.Exec(`CREATE TRIGGER tombstone_fail BEFORE INSERT ON message_tombstones
			BEGIN SELECT RAISE(ABORT, 'forced tombstone failure'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
		if _, err := d.PurgeExpiredMessages(0); err == nil {
			t.Fatalf("purge must fail when the tombstone insert fails")
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM messages WHERE id = ?`, m.ID); n != 1 {
			t.Fatalf("message deleted although its tombstone failed")
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM deliveries WHERE message_id = ?`, m.ID); n == 0 {
			t.Fatalf("deliveries deleted although the purge tx failed")
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones`); n != 0 {
			t.Fatalf("%d tombstones landed from a failed tx", n)
		}
	})

	t.Run("P0AndTTL0NeverTombstoned", func(t *testing.T) {
		d := retentionTestDB(t)
		p0, err := d.InsertMessage("p1", "a", "b", "notification", "s", "fire", "{}", "P0", 60, nil, nil)
		if err != nil {
			t.Fatalf("insert P0: %v", err)
		}
		forever, err := d.InsertMessage("p1", "a", "b", "notification", "s", "keep", "{}", "P2", 0, nil, nil)
		if err != nil {
			t.Fatalf("insert ttl0: %v", err)
		}
		normal, err := d.InsertMessage("p1", "a", "b", "notification", "s", "normal", "{}", "P2", 60, nil, nil)
		if err != nil {
			t.Fatalf("insert normal: %v", err)
		}
		longAgo := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(memoryTimeFmt)
		if _, err := d.conn.Exec(`UPDATE messages SET created_at = ?`, longAgo); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		if _, err := d.ExpireMessages(); err != nil {
			t.Fatalf("expire: %v", err)
		}
		// Push the normal message's expiry past the grace; the sweep never
		// marked P0/ttl=0, so the purge predicate can't reach them.
		expireLongAgo(t, d, normal.ID)
		if _, err := d.PurgeExpiredMessages(0); err != nil {
			t.Fatalf("purge: %v", err)
		}
		for _, id := range []string{p0.ID, forever.ID} {
			if n := countRows(t, d, `SELECT COUNT(*) FROM messages WHERE id = ?`, id); n != 1 {
				t.Fatalf("message %s purged; P0/ttl=0 must be kept", id)
			}
			if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones WHERE id = ?`, id); n != 0 {
				t.Fatalf("message %s tombstoned; P0/ttl=0 must never be", id)
			}
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones WHERE id = ?`, normal.ID); n != 1 {
			t.Fatalf("control message not tombstoned")
		}
	})

	t.Run("InboxUnchanged", func(t *testing.T) {
		d := retentionTestDB(t)
		gone, err := d.InsertMessage("p1", "a", "b", "notification", "s", "gone", "{}", "P2", 3600, nil, nil)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, _, err := d.InsertMessageWithDeliveries("p1", "a", "b", "notification", "s", "live", "{}", "P2", 3600, nil, nil, []string{"b"}, ""); err != nil {
			t.Fatalf("insert live: %v", err)
		}
		expireLongAgo(t, d, gone.ID)
		if _, err := d.PurgeExpiredMessages(0); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM message_tombstones`); n != 1 {
			t.Fatalf("fixture: want 1 tombstone, got %d", n)
		}
		before, err := d.UnreadCountForAgent("p1", "b")
		if err != nil {
			t.Fatalf("unread: %v", err)
		}
		inbox, err := d.GetInbox("p1", "b", false, 100)
		if err != nil {
			t.Fatalf("inbox: %v", err)
		}
		if len(inbox) != 1 || inbox[0].ID == gone.ID {
			t.Fatalf("inbox must hold only the live message, got %d", len(inbox))
		}
		// Clearing the tombstones must not change what the inbox shows.
		if _, err := d.conn.Exec(`DELETE FROM message_tombstones`); err != nil {
			t.Fatalf("clear: %v", err)
		}
		after, err := d.UnreadCountForAgent("p1", "b")
		if err != nil {
			t.Fatalf("unread: %v", err)
		}
		inbox2, err := d.GetInbox("p1", "b", false, 100)
		if err != nil {
			t.Fatalf("inbox: %v", err)
		}
		if before != after || len(inbox2) != len(inbox) || inbox2[0].ID != inbox[0].ID {
			t.Fatalf("inbox/unread changed with tombstones: unread %d vs %d, inbox %d vs %d", before, after, len(inbox), len(inbox2))
		}
	})

	t.Run("OldDBMigrates", func(t *testing.T) {
		d := retentionTestDB(t)
		if _, err := d.conn.Exec(`DROP TABLE message_tombstones`); err != nil {
			t.Fatalf("drop: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := migrate(d.conn); err != nil {
				t.Fatalf("migrate run %d: %v", i+1, err)
			}
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'message_tombstones'`); n != 1 {
			t.Fatalf("message_tombstones not recreated by migrate")
		}
		if n := countRows(t, d, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_message_tombstones_reply'`); n != 1 {
			t.Fatalf("tombstone reply index missing after migrate")
		}
	})
}
