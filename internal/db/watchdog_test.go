package db

import (
	"path/filepath"
	"testing"
	"time"
)

func newWatchdogDB(t *testing.T) *DB {
	t.Helper()
	d, err := NewTestDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// backdateLastSeen forces an agent's last_seen to `ago` in the past so the
// idle-threshold logic can be exercised deterministically (no sleeping).
func backdateLastSeen(t *testing.T, d *DB, project, name string, ago time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-ago).Format(time.RFC3339)
	if _, err := d.conn.Exec("UPDATE agents SET last_seen = ? WHERE name = ? AND project = ?", ts, name, project); err != nil {
		t.Fatalf("backdate last_seen: %v", err)
	}
}

func registerHeldTask(t *testing.T, d *DB, project, agent string) string {
	t.Helper()
	if _, _, err := d.RegisterAgent(project, agent, "", "", nil, nil, false, nil, "", 0, RegisterOptions{}); err != nil {
		t.Fatalf("register %s: %v", agent, err)
	}
	task, err := d.DispatchTask(project, "prof", "lead", "held work", "", "P2", nil, nil, TypedTicket{}, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := d.ClaimTask(task.ID, agent, project); err != nil {
		t.Fatalf("claim: %v", err)
	}
	return task.ID
}

func TestStuckAgents(t *testing.T) {
	d := newWatchdogDB(t)
	const project = "p1"

	// Silent holder: past threshold, holding an in-flight task → a candidate.
	stuckTask := registerHeldTask(t, d, project, "stuck")
	backdateLastSeen(t, d, project, "stuck", 40*time.Minute)

	// Live holder: seen recently, holding a task → NOT a candidate.
	registerHeldTask(t, d, project, "live")

	// Silent but idle (no held task) → NOT a candidate.
	if _, _, err := d.RegisterAgent(project, "idle", "", "", nil, nil, false, nil, "", 0, RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	backdateLastSeen(t, d, project, "idle", 40*time.Minute)

	got, err := d.StuckAgents(project, DefaultStuckThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 stuck agent, got %d (%v)", len(got), got)
	}
	sa := got[0]
	if sa.Agent != "stuck" {
		t.Fatalf("wrong candidate: %s", sa.Agent)
	}
	if len(sa.Tasks) != 1 || sa.Tasks[0].ID != stuckTask {
		t.Fatalf("expected held task %s, got %v", stuckTask, sa.Tasks)
	}
	if sa.IdleSeconds < int64((30 * time.Minute).Seconds()) {
		t.Fatalf("idle_seconds too small: %d", sa.IdleSeconds)
	}
}

// TestStuckAgents_ExcludesParkedAndDeleted: the query scopes to status IN
// ('active','inactive'), so a 'sleeping' (intentionally parked) or a 'deleted'
// (tombstoned) holder is NOT a stuck candidate even while a task still points at
// it. ('inactive' — set by BOTH the stale sweep AND graceful DeactivateAgent —
// IS a candidate; see TestStuckAgents_IncludesInactive.)
func TestStuckAgents_ExcludesParkedAndDeleted(t *testing.T) {
	d := newWatchdogDB(t)
	const project = "p1"
	for _, tc := range []struct{ agent, status string }{
		{"parked", "sleeping"},
		{"tombstoned", "deleted"},
	} {
		registerHeldTask(t, d, project, tc.agent)
		backdateLastSeen(t, d, project, tc.agent, 40*time.Minute)
		if _, err := d.conn.Exec("UPDATE agents SET status = ? WHERE name = ? AND project = ?", tc.status, tc.agent, project); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.StuckAgents(project, DefaultStuckThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("sleeping/deleted holders must be excluded, got %v", got)
	}
}

// TestStuckAgents_IncludesInactive: a silent holder swept to 'inactive' (lease
// lapsed) — the same status graceful DeactivateAgent sets — is still a requeue
// candidate. Locks the documented status scope so the doc/test can't drift again.
func TestStuckAgents_IncludesInactive(t *testing.T) {
	d := newWatchdogDB(t)
	const project = "p1"
	registerHeldTask(t, d, project, "swept")
	backdateLastSeen(t, d, project, "swept", 40*time.Minute)
	if _, err := d.conn.Exec("UPDATE agents SET status = 'inactive' WHERE name = ? AND project = ?", "swept", project); err != nil {
		t.Fatal(err)
	}
	got, err := d.StuckAgents(project, DefaultStuckThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Agent != "swept" {
		t.Fatalf("inactive holder must be a candidate, got %v", got)
	}
}

func TestRequeueTask(t *testing.T) {
	d := newWatchdogDB(t)
	const project = "p1"
	taskID := registerHeldTask(t, d, project, "stuck")
	// Move it to in-progress to exercise a mid-flight requeue.
	if _, err := d.StartTask(taskID, "stuck", project); err != nil {
		t.Fatalf("start: %v", err)
	}

	task, err := d.RequeueTask(taskID, project, "")
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if task.Status != "pending" {
		t.Fatalf("expected pending, got %q", task.Status)
	}
	if task.AssignedTo != nil {
		t.Fatalf("assignee must be cleared, got %v", *task.AssignedTo)
	}
	if task.LeaseHolder != nil {
		t.Fatalf("lease holder must be cleared, got %v", *task.LeaseHolder)
	}
	if task.LeaseTransfer == nil || task.LeaseTransfer.From != "stuck" || task.LeaseTransfer.Reason != "stuck-watchdog" {
		t.Fatalf("expected release transfer from stuck/stuck-watchdog, got %v", task.LeaseTransfer)
	}

	// A fresh agent can now claim the requeued task.
	if _, err := d.ClaimTask(taskID, "rescuer", project); err != nil {
		t.Fatalf("re-claim after requeue: %v", err)
	}
}

// TestRequeueTask_RefusesTerminal: a done task is not requeuable (no
// resurrection of finished work).
func TestRequeueTask_RefusesTerminal(t *testing.T) {
	d := newWatchdogDB(t)
	const project = "p1"
	taskID := registerHeldTask(t, d, project, "stuck")
	if _, err := d.CompleteTask(taskID, "stuck", project, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := d.RequeueTask(taskID, project, ""); err == nil {
		t.Fatal("expected requeue of a done task to be refused")
	}
}
