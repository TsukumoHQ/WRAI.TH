package db

import (
	"testing"
	"time"
)

// backdateDispatchedAt pushes a task's dispatched_at into the past so it reads
// as a genuine unacked candidate for GetUnackedTasks.
func backdateDispatchedAt(t *testing.T, d *DB, taskID string, age time.Duration) {
	t.Helper()
	old := time.Now().UTC().Add(-age).Format(memoryTimeFmt)
	if _, err := d.conn.Exec("UPDATE tasks SET dispatched_at = ? WHERE id = ?", old, taskID); err != nil {
		t.Fatalf("backdate dispatched_at: %v", err)
	}
}

// S2 (6641e3ad): a run container deliberately stays status='pending' while its
// run_state advances — GetUnackedTasks must never surface it as a candidate, or
// the ACK checker nags/escalates a container that was never meant to be claimed.
func TestGetUnackedTasksExcludesRunContainers(t *testing.T) {
	d := testDB(t)
	plain, _ := d.DispatchTask("proj", "lead", "cto", "plain", "", "P1", nil, nil, TypedTicket{}, false)
	container, _ := d.DispatchTask("proj", "lead", "cto", "container", "", "P1", nil, nil, TypedTicket{}, false)
	backdateDispatchedAt(t, d, plain.ID, time.Hour)
	backdateDispatchedAt(t, d, container.ID, time.Hour)

	if _, err := d.SetTaskRun(container.ID, "proj", nil, strptr(RunStateOpen)); err != nil {
		t.Fatalf("set run: %v", err)
	}

	batch, err := d.GetUnackedTasks(30 * time.Minute)
	if err != nil {
		t.Fatalf("get unacked: %v", err)
	}
	if len(batch) != 1 || batch[0].ID != plain.ID {
		t.Fatalf("expected only the plain task, got %+v", batch)
	}
}

// S2 (6641e3ad): the ACK checker's read (GetUnackedTasks) and act (Mark*) are
// two separate calls, not one transaction — a run can start on the task in
// between (it "passes into" container/terminal territory mid-scan). The mark
// call must refuse (ok=false) instead of firing a notification for state that
// is no longer true — this is the CAS guard that closes that window.
func TestAckMarkNoOpsWhenTaskBecomesRunContainerMidScan(t *testing.T) {
	d := testDB(t)
	p, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false)
	backdateDispatchedAt(t, d, p.ID, time.Hour)

	// The scanner's read: task is a plain unacked pending task (no run_state).
	batch, err := d.GetUnackedTasks(30 * time.Minute)
	if err != nil {
		t.Fatalf("get unacked: %v", err)
	}
	if len(batch) != 1 || batch[0].ID != p.ID {
		t.Fatalf("expected the task in the unacked batch, got %+v", batch)
	}

	// Race: a run starts on the task AFTER the scanner's read but BEFORE it acts.
	if _, err := d.SetTaskRun(p.ID, "proj", nil, strptr(RunStateOpen)); err != nil {
		t.Fatalf("set run: %v", err)
	}

	// The scanner's act, using the now-stale batch entry, must no-op.
	if ok, err := d.MarkTaskAckNotified(p.ID); err != nil {
		t.Fatalf("mark notified: %v", err)
	} else if ok {
		t.Fatal("MarkTaskAckNotified must no-op (ok=false) once the task became a run container")
	}
	if ok, err := d.MarkTaskAckEscalated(p.ID); err != nil {
		t.Fatalf("mark escalated: %v", err)
	} else if ok {
		t.Fatal("MarkTaskAckEscalated must no-op (ok=false) once the task became a run container")
	}

	final, _ := d.GetTask(p.ID, "proj")
	if final.AckNotifiedAt != nil || final.AckEscalatedAt != nil {
		t.Fatalf("ack timestamps must stay unset on a no-op, got notified=%v escalated=%v", final.AckNotifiedAt, final.AckEscalatedAt)
	}
}

// The same CAS guard also protects against a plain double-fire: two concurrent
// ticks of the checker must not both mark+notify the same task.
func TestAckMarkIdempotentAcrossConcurrentTicks(t *testing.T) {
	d := testDB(t)
	p, _ := d.DispatchTask("proj", "lead", "cto", "run", "", "P1", nil, nil, TypedTicket{}, false)
	backdateDispatchedAt(t, d, p.ID, time.Hour)

	ok1, err := d.MarkTaskAckNotified(p.ID)
	if err != nil || !ok1 {
		t.Fatalf("first mark should succeed: ok=%v err=%v", ok1, err)
	}
	ok2, err := d.MarkTaskAckNotified(p.ID)
	if err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if ok2 {
		t.Fatal("second concurrent mark must no-op (ok=false), not re-fire")
	}
}
