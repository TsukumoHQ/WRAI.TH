package db

import (
	"testing"
	"time"
)

// Task 58ece5e2: the three raw UPDATEs that put a task back to 'pending'
// outside transitionTask (requeue, lease-expiry release, agent-deactivation
// cascade) restart the ACK clock, like transitionTask does since c933b2f1.

// agePendingSince moves a task's dispatched_at and pending_since 30 days back
// and returns that timestamp.
func agePendingSince(t *testing.T, d *DB, taskID string) string {
	t.Helper()
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(memoryTimeFmt)
	if _, err := d.conn.Exec(`UPDATE tasks SET dispatched_at = ?, pending_since = ? WHERE id = ?`, old, old, taskID); err != nil {
		t.Fatalf("age %s: %v", taskID, err)
	}
	return old
}

// assertClockRestarted checks the task is pending and its pending_since was
// stamped by the release (equal to the last_activity_at the same UPDATE set),
// not left at old.
func assertClockRestarted(t *testing.T, d *DB, taskID, old string) {
	t.Helper()
	var status, since, activity string
	if err := d.conn.QueryRow(`SELECT status, COALESCE(pending_since, ''), COALESCE(last_activity_at, '') FROM tasks WHERE id = ?`, taskID).
		Scan(&status, &since, &activity); err != nil {
		t.Fatalf("read %s: %v", taskID, err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
	if since == old || since != activity {
		t.Fatalf("pending_since = %q (old %q), want the release time %q", since, old, activity)
	}
}

// TestPendingSinceStampedOnRequeue (AC1): RequeueTask restarts the clock, so a
// sweep one minute later opens no obligation at all.
func TestPendingSinceStampedOnRequeue(t *testing.T) {
	d := testDB(t)
	id := dispatchClaimed(t, d, "p1", "worker")
	old := agePendingSince(t, d, id)

	if _, err := d.RequeueTask(id, "p1", "test"); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	requeuedAt := time.Now().UTC()
	assertClockRestarted(t, d, id, old)

	at := requeuedAt.Add(time.Minute)
	if _, err := d.InstantiateTaskAck(at.Add(-15*time.Minute), at); err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM obligations WHERE subject_id = ?`, id); n != 0 {
		t.Fatalf("requeue+1min opened %d obligation(s), want 0", n)
	}
}

// TestPendingSinceStampedOnLeaseExpiry (AC2): the lease sweep's release of a
// dead holder's expired lease restarts the clock.
func TestPendingSinceStampedOnLeaseExpiry(t *testing.T) {
	d := testDB(t)
	id := dispatchClaimed(t, d, "p1", "dead-holder")
	if _, err := d.StartTask(id, "dead-holder", "p1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	old := agePendingSince(t, d, id)
	forceLeaseExpired(t, d, id, "p1")
	if err := d.DeactivateAgent("p1", "dead-holder"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	swept, err := d.SweepExpiredLeases()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].TaskID != id {
		t.Fatalf("swept = %+v, want %s", swept, id)
	}
	assertClockRestarted(t, d, id, old)
}

// TestPendingSinceStampedOnOrphanCascade (AC3): releasing a removed agent's
// leased task through CascadeAgentDeactivation restarts the clock.
func TestPendingSinceStampedOnOrphanCascade(t *testing.T) {
	d := testDB(t)
	id := dispatchClaimed(t, d, "p1", "leaver")
	old := agePendingSince(t, d, id)

	out, err := d.CascadeAgentDeactivation("p1", "leaver")
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if len(out.Released) != 1 || out.Released[0].TaskID != id {
		t.Fatalf("released = %+v, want %s", out.Released, id)
	}
	assertClockRestarted(t, d, id, old)
}
