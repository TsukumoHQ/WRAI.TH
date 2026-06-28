package db

import (
	"path/filepath"
	"testing"
	"time"
)

func retentionTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := NewTestDB(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatalf("NewTestDB: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func countRows(t *testing.T, d *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.conn.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", q, err)
	}
	return n
}

// PurgeExpiredMessages hard-deletes messages soft-expired longer than the grace
// window, along with their deliveries and read receipts — but leaves messages
// still inside the grace window (and never-expired ttl=0 messages) untouched.
func TestPurgeExpiredMessages(t *testing.T) {
	d := retentionTestDB(t)

	// Three messages, each gets a delivery row from InsertMessage.
	old, err := d.InsertMessage("default", "a", "b", "notification", "s", "old", "{}", "P2", 3600, nil, nil)
	if err != nil {
		t.Fatalf("insert old: %v", err)
	}
	recent, err := d.InsertMessage("default", "a", "b", "notification", "s", "recent", "{}", "P2", 3600, nil, nil)
	if err != nil {
		t.Fatalf("insert recent: %v", err)
	}
	persistent, err := d.InsertMessage("default", "a", "b", "notification", "s", "persistent", "{}", "P2", 0, nil, nil)
	if err != nil {
		t.Fatalf("insert persistent: %v", err)
	}

	// A read receipt on the old message, so we can confirm it's reclaimed too.
	if _, err := d.conn.Exec(
		`INSERT INTO message_reads (message_id, agent_name, project, read_at) VALUES (?, 'b', 'default', ?)`,
		old.ID, time.Now().UTC().Format(memoryTimeFmt)); err != nil {
		t.Fatalf("seed read receipt: %v", err)
	}

	// Soft-expire: old = 10 days ago (past the 7d grace), recent = 1 day ago (within).
	tenDaysAgo := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(memoryTimeFmt)
	oneDayAgo := time.Now().UTC().Add(-1 * 24 * time.Hour).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(`UPDATE messages SET expired_at = ? WHERE id = ?`, tenDaysAgo, old.ID); err != nil {
		t.Fatalf("expire old: %v", err)
	}
	if _, err := d.conn.Exec(`UPDATE messages SET expired_at = ? WHERE id = ?`, oneDayAgo, recent.ID); err != nil {
		t.Fatalf("expire recent: %v", err)
	}

	purged, err := d.PurgeExpiredMessages(7 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpiredMessages: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (only the message expired past grace)", purged)
	}

	// Old message + its children are gone.
	if n := countRows(t, d, `SELECT COUNT(*) FROM messages WHERE id = ?`, old.ID); n != 0 {
		t.Errorf("old message not purged: %d rows", n)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM deliveries WHERE message_id = ?`, old.ID); n != 0 {
		t.Errorf("old deliveries not purged: %d rows", n)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM message_reads WHERE message_id = ?`, old.ID); n != 0 {
		t.Errorf("old read receipts not purged: %d rows", n)
	}
	// Recent (within grace) and persistent (ttl=0) survive.
	if n := countRows(t, d, `SELECT COUNT(*) FROM messages WHERE id IN (?, ?)`, recent.ID, persistent.ID); n != 2 {
		t.Errorf("recent/persistent messages wrongly purged: %d of 2 survive", n)
	}
}

// PurgeOldAuditLog deletes audit entries older than maxAge, keeps newer ones.
func TestPurgeOldAuditLog(t *testing.T) {
	d := retentionTestDB(t)

	if err := d.RecordAudit(auditEntry("p1", "user", "transition", "task-old", "old")); err != nil {
		t.Fatalf("record old: %v", err)
	}
	if err := d.RecordAudit(auditEntry("p1", "user", "transition", "task-new", "new")); err != nil {
		t.Fatalf("record new: %v", err)
	}
	// Backdate the old entry past the 90d window.
	oldTS := time.Now().UTC().Add(-100 * 24 * time.Hour).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(`UPDATE audit_log SET created_at = ? WHERE resource_id = 'task-old'`, oldTS); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	purged, err := d.PurgeOldAuditLog(90 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PurgeOldAuditLog: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit_log WHERE resource_id = 'task-old'`); n != 0 {
		t.Errorf("old audit row not purged: %d", n)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit_log WHERE resource_id = 'task-new'`); n != 1 {
		t.Errorf("recent audit row wrongly purged: %d", n)
	}
}
