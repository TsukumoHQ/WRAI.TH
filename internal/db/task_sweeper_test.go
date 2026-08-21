package db

import (
	"testing"
)

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

// setPRState links a PR and forces the persisted pr_state + task status a test
// wants (setup shortcut; mirrors what the webhook/poll path would have left).
func setPRState(t *testing.T, d *DB, taskID, project, prState, status string) {
	t.Helper()
	if _, err := d.SetTaskPR(taskID, project, strp("https://x/pr/1"), intp(1), strp(prState), strp("org/repo")); err != nil {
		t.Fatalf("set pr: %v", err)
	}
	if _, err := d.conn.Exec("UPDATE tasks SET status = ? WHERE id = ? AND project = ?", status, taskID, project); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

// G3: a task whose PR is already merged/closed but whose status never converged
// is stranded — the open-only reconcile set can't see it. ListStrandedPRTasks
// surfaces exactly those, excluding terminal and already-settled rows.
func TestListStrandedPRTasks(t *testing.T) {
	d := testDB(t)
	mk := func(title, prState, status string) string {
		task, err := d.DispatchTask("p1", "dev", "cto", title, "", "P2", nil, nil, TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
		setPRState(t, d, task.ID, "p1", prState, status)
		return task.ID
	}

	mergedStranded := mk("merged-stranded", "merged", "in-review")
	closedStranded := mk("closed-stranded", "closed", "in-review")
	mk("merged-done", "merged", "done")       // terminal — not stranded
	mk("closed-settled", "closed", "blocked") // already converged — excluded
	mk("open-inflight", "open", "in-review")  // open — legitimately in flight

	got, err := d.ListStrandedPRTasks(100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, tk := range got {
		ids[tk.ID] = true
	}
	if len(got) != 2 || !ids[mergedStranded] || !ids[closedStranded] {
		t.Fatalf("expected exactly the 2 stranded tasks, got %d: %+v", len(got), ids)
	}
}

// G3: converging a stranded merged task (via ForcePRTransition, the same path the
// sweeper calls) moves it to done; a terminal task is never resurrected.
func TestStrandedPRConvergence(t *testing.T) {
	d := testDB(t)
	task, err := d.DispatchTask("p1", "dev", "cto", "merged", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	setPRState(t, d, task.ID, "p1", "merged", "in-review")

	got, changed, err := d.ForcePRTransition("p1", task.ID, "done", nil)
	if err != nil || !changed || got.Status != "done" {
		t.Fatalf("merged should converge to done: changed=%v status=%v err=%v", changed, got.Status, err)
	}
	// No-resurrect: a second convergence is an idempotent no-op.
	if _, changed2, _ := d.ForcePRTransition("p1", task.ID, "done", nil); changed2 {
		t.Fatalf("second convergence should be a no-op")
	}
}

// G4: a task held by a DEAD agent past lease expiry is requeued to pending; a
// LIVE holder's expired lease is left alone; the sweep is idempotent.
func TestSweepExpiredLeases(t *testing.T) {
	d := testDB(t)

	// Dead holder, expired lease, in-progress → recovered.
	deadID := dispatchClaimed(t, d, "p1", "dead-holder")
	if _, err := d.StartTask(deadID, "dead-holder", "p1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	forceLeaseExpired(t, d, deadID, "p1")
	if err := d.DeactivateAgent("p1", "dead-holder"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// Live holder, expired lease → NOT swept (generous TTL, not a liveness probe).
	liveID := dispatchClaimed(t, d, "p1", "live-holder")
	forceLeaseExpired(t, d, liveID, "p1")

	swept, err := d.SweepExpiredLeases()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].TaskID != deadID || swept[0].From != "dead-holder" {
		t.Fatalf("expected only the dead-holder task recovered, got %+v", swept)
	}

	dead, _ := d.GetTask(deadID, "p1")
	if dead.Status != "pending" || strVal(dead.LeaseHolder) != "" {
		t.Fatalf("recovered task should be pending with no holder, got status=%q holder=%q", dead.Status, strVal(dead.LeaseHolder))
	}
	live, _ := d.GetTask(liveID, "p1")
	if live.Status == "pending" {
		t.Fatalf("live holder's task must NOT be swept")
	}

	// Idempotent: the requeued task is no longer a candidate.
	swept2, err := d.SweepExpiredLeases()
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(swept2) != 0 {
		t.Fatalf("second sweep should recover nothing, got %+v", swept2)
	}
}

// G4 no-resurrect: a terminal (done/cancelled) task with a dead holder + expired
// lease is never swept back to pending.
func TestSweepExpiredLeases_NoResurrectTerminal(t *testing.T) {
	d := testDB(t)
	id := dispatchClaimed(t, d, "p1", "dead")
	if _, err := d.CompleteTask(id, "dead", "p1", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Backdate the (now stale) lease columns and kill the holder.
	if _, err := d.conn.Exec(
		"UPDATE tasks SET lease_holder='dead', lease_expires_at='2020-01-01T00:00:00Z' WHERE id=? AND project='p1'", id,
	); err != nil {
		t.Fatalf("age lease: %v", err)
	}
	if err := d.DeactivateAgent("p1", "dead"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	swept, err := d.SweepExpiredLeases()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 0 {
		t.Fatalf("terminal task must never be swept, got %+v", swept)
	}
	tk, _ := d.GetTask(id, "p1")
	if tk.Status != "done" {
		t.Fatalf("terminal task status changed to %q", tk.Status)
	}
}
