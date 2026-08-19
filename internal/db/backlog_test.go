package db

import (
	"errors"
	"testing"
)

// A backlog task is groomed-but-not-claimable: dispatch-to-backlog lands in
// 'backlog', a claim is refused (typed conflict, not a silent accept), promote
// lifts it to pending, and only then does a claim succeed.
func TestBacklogLifecycle(t *testing.T) {
	d := testDB(t)

	task, err := d.DispatchTask("p", "dev", "cto", "groomed", "", "P2", nil, nil, TypedTicket{}, true)
	if err != nil {
		t.Fatalf("dispatch backlog: %v", err)
	}
	if task.Status != "backlog" {
		t.Fatalf("dispatch backlog status = %q, want backlog", task.Status)
	}

	// Not claimable while in backlog.
	if _, err := d.ClaimTask(task.ID, "worker", "p"); err == nil {
		t.Fatal("claim on a backlog task must be refused")
	} else {
		var te *TaskError
		if !errors.As(err, &te) || te.Code != CodeTaskStateConflict {
			t.Fatalf("claim refusal should be a typed TASK_STATE_CONFLICT, got %v", err)
		}
	}
	// Still backlog (the failed claim didn't move it).
	if got, _ := d.GetTask(task.ID, "p"); got.Status != "backlog" {
		t.Fatalf("status after refused claim = %q, want backlog", got.Status)
	}

	// Promote → pending (changed=true, it really moved).
	promoted, changed, err := d.PromoteTask(task.ID, "cto", "p")
	if err != nil || !changed || promoted.Status != "pending" {
		t.Fatalf("promote → pending failed: status=%v changed=%v err=%v", promoted.Status, changed, err)
	}

	// Now claimable.
	claimed, err := d.ClaimTask(task.ID, "worker", "p")
	if err != nil || claimed.Status != "accepted" {
		t.Fatalf("claim after promote failed: status=%v err=%v", claimed.Status, err)
	}
}

// A normal (non-backlog) dispatch is pending and immediately claimable — the
// existing path is unchanged.
func TestDispatchDefaultPendingClaimable(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p", "dev", "cto", "normal", "", "P2", nil, nil, TypedTicket{}, false)
	if err != nil || task.Status != "pending" {
		t.Fatalf("default dispatch status = %v, err = %v; want pending", task.Status, err)
	}
	if _, err := d.ClaimTask(task.ID, "worker", "p"); err != nil {
		t.Fatalf("pending task must be claimable: %v", err)
	}
}

// promote_task is lifecycle-enforced: it is a no-op on an already-pending task
// and refuses to pull an in-progress task back to pending.
func TestPromoteLifecycleEnforced(t *testing.T) {
	d := testDB(t)
	task, _ := d.DispatchTask("p", "dev", "cto", "t", "", "P2", nil, nil, TypedTicket{}, false)
	// pending → promote is an idempotent no-op: still pending, no error, changed=false
	// so the caller doesn't re-announce (double-promote must not re-wake the fleet).
	if got, changed, err := d.PromoteTask(task.ID, "cto", "p"); err != nil || changed || got.Status != "pending" {
		t.Fatalf("promote on pending should be a no-op (changed=false): status=%v changed=%v err=%v", got.Status, changed, err)
	}
	// in-progress → promote is an invalid transition.
	if _, err := d.ClaimTask(task.ID, "worker", "p"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := d.StartTask(task.ID, "worker", "p"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, _, err := d.PromoteTask(task.ID, "cto", "p"); err == nil {
		t.Fatal("promote on an in-progress task must be refused")
	}
}
