package db

import (
	"fmt"
	"testing"
	"time"
)

// insertDeadletter injects a deadletter row with a controlled priority and
// capture age (days in the past) so the retention sweep can be exercised
// deterministically without waiting real time.
func insertDeadletter(t *testing.T, d *DB, id, priority string, ageDays int) {
	t.Helper()
	ts := time.Now().UTC().AddDate(0, 0, -ageDays).Format("2006-01-02T15:04:05.000000Z")
	_, err := d.conn.Exec(
		`INSERT INTO deadletter (id, message_id, to_agent, from_agent, priority, subject, project, created_at, expired_at)
		 VALUES (?, ?, 'bot-a', 'sender', ?, 's', 'p1', ?, ?)`,
		id, "msg-"+id, priority, ts, ts,
	)
	if err != nil {
		t.Fatalf("insert deadletter %s: %v", id, err)
	}
}

func deadletterCount(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.ro().QueryRow("SELECT COUNT(*) FROM deadletter").Scan(&n); err != nil {
		t.Fatalf("count deadletter: %v", err)
	}
	return n
}

// Self-GC: aged non-P0/P1 rows are trimmed past the short horizon; P0/P1 are
// kept until the long horizon; recent rows are retained; the sweep is
// idempotent. (aa3c53fe)
func TestPurgeOldDeadletter(t *testing.T) {
	d := testDB(t)

	short := 30 * 24 * time.Hour
	long := 180 * 24 * time.Hour

	insertDeadletter(t, d, "p3-old", "P3", 40)  // trim (non-P0/P1, past short)
	insertDeadletter(t, d, "p2-old", "P2", 40)  // trim
	insertDeadletter(t, d, "p2-new", "P2", 10)  // keep (within short)
	insertDeadletter(t, d, "p0-mid", "P0", 40)  // keep (P0, within long)
	insertDeadletter(t, d, "p1-mid", "P1", 100) // keep (P1, within long)
	insertDeadletter(t, d, "p0-anc", "P0", 200) // trim (P0, past long)

	purged, err := d.PurgeOldDeadletter(short, long)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 3 {
		t.Fatalf("expected 3 trimmed (p3-old, p2-old, p0-anc), got %d", purged)
	}
	if got := deadletterCount(t, d); got != 3 {
		t.Fatalf("expected 3 retained (p2-new, p0-mid, p1-mid), got %d", got)
	}

	// The right rows survived.
	for _, id := range []string{"p2-new", "p0-mid", "p1-mid"} {
		var c int
		_ = d.ro().QueryRow("SELECT COUNT(*) FROM deadletter WHERE id = ?", id).Scan(&c)
		if c != 1 {
			t.Errorf("expected %s retained", id)
		}
	}

	// Idempotent: a second sweep with nothing newly aged trims nothing.
	again, err := d.PurgeOldDeadletter(short, long)
	if err != nil {
		t.Fatalf("purge 2: %v", err)
	}
	if again != 0 {
		t.Errorf("second sweep must be idempotent, trimmed %d", again)
	}
}

// The batch loop reclaims a backlog larger than one batch in full.
func TestPurgeOldDeadletterBatches(t *testing.T) {
	d := testDB(t)
	total := deadletterPurgeBatch + 50
	for i := 0; i < total; i++ {
		insertDeadletter(t, d, fmt.Sprintf("row-%d", i), "P3", 40)
	}
	purged, err := d.PurgeOldDeadletter(24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged <= deadletterPurgeBatch {
		t.Fatalf("batch loop should reclaim the whole backlog, got %d", purged)
	}
	if got := deadletterCount(t, d); got != 0 {
		t.Fatalf("expected all aged rows trimmed, %d remain", got)
	}
}
