package db

import (
	"errors"
	"strings"
	"testing"
)

// Task e2273dc3 — WIP limit: an agent holds at most wip_limit (project
// setting, default 1) accepted / in-progress tasks; in-review does not count.

func wantWIP(t *testing.T, err error, heldID, what string) {
	t.Helper()
	var te *TaskError
	if !errors.As(err, &te) || te.Code != CodeWIPLimit {
		t.Fatalf("%s: want %s, got %v", what, CodeWIPLimit, err)
	}
	if heldID != "" && !strings.Contains(te.Msg, heldID) {
		t.Fatalf("%s: refusal must name the held task %s: %q", what, heldID, te.Msg)
	}
}

func dispatchPending(t *testing.T, d *DB, project, title string) string {
	t.Helper()
	task, err := d.DispatchTask(project, "", "dispatcher", title, "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch %s: %v", title, err)
	}
	return task.ID
}

// AC1 + start: holding one accepted task, claim and start of another are
// refused naming the held task; starting the held task itself is fine.
func TestWIPClaimAndStartRefusedWhileHolding(t *testing.T) {
	d := testDB(t)
	const p, a = "p1", "worker-a"
	held := dispatchClaimed(t, d, p, a)
	t2 := dispatchPending(t, d, p, "second")

	_, err := d.ClaimTask(t2, a, p)
	wantWIP(t, err, held, "claim while holding one")
	_, err = d.StartTask(t2, a, p)
	wantWIP(t, err, held, "start another while holding one")
	mustStatus(t, d, t2, p, "pending")

	if _, err := d.StartTask(held, a, p); err != nil {
		t.Fatalf("start the held task itself: %v", err)
	}
}

// AC2: claim_task next refuses the same way.
func TestWIPClaimNextRefused(t *testing.T) {
	d := testDB(t)
	const p, a = "p1", "worker-a"
	held := dispatchClaimed(t, d, p, a)
	dispatchPending(t, d, p, "second")
	_, err := d.ClaimNextTask(p, a, "", "", SortPriority)
	wantWIP(t, err, held, "claim next while holding one")
}

// AC3 + AC4: in-review frees the slot; a bounce back to in-progress (gate
// daemon block + resume) counts again, so the next claim is refused.
func TestWIPInReviewFreesSlotBounceCountsAgain(t *testing.T) {
	d := testDB(t)
	const p, a = "p1", "worker-a"
	t1 := dispatchClaimed(t, d, p, a)
	if _, err := d.StartTask(t1, a, p); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := d.ReviewTask(t1, a, p); err != nil {
		t.Fatalf("review: %v", err)
	}
	t2 := dispatchPending(t, d, p, "second")
	if _, err := d.ClaimTask(t2, a, p); err != nil {
		t.Fatalf("claim next while previous is in review: %v", err)
	}
	if _, err := d.StartTask(t2, a, p); err != nil {
		t.Fatalf("start it: %v", err)
	}

	// Gate rejects t1: niwa blocks then resumes it; t1 is the doer's again.
	if _, err := d.BlockTask(t1, "niwa", p, strptr("gate rejected")); err != nil {
		t.Fatalf("niwa block: %v", err)
	}
	if _, err := d.StartTask(t1, "niwa", p); err != nil {
		t.Fatalf("niwa resume must not be WIP-refused: %v", err)
	}
	t3 := dispatchPending(t, d, p, "third")
	_, err := d.ClaimTask(t3, a, p)
	wantWIP(t, err, t1, "claim after a bounce")
}

// AC5: wip_limit is per project, default 1; 0 = unlimited; 2 allows two.
func TestWIPLimitProjectSetting(t *testing.T) {
	d := testDB(t)
	const p, a = "p1", "worker-a"
	regAgent(t, d, p, a)
	if got := d.ProjectWIPLimit(p); got != 1 {
		t.Fatalf("default wip_limit: want 1, got %d", got)
	}
	if err := d.SetProjectWIPLimit(p, -1); err == nil {
		t.Fatal("negative wip_limit must be refused")
	}

	if err := d.SetProjectWIPLimit(p, 2); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	first := dispatchClaimed(t, d, p, a)
	if _, err := d.ClaimTask(dispatchPending(t, d, p, "two"), a, p); err != nil {
		t.Fatalf("limit 2: second claim: %v", err)
	}
	_, err := d.ClaimTask(dispatchPending(t, d, p, "three"), a, p)
	wantWIP(t, err, first, "limit 2: third claim")

	if err := d.SetProjectWIPLimit(p, 0); err != nil {
		t.Fatalf("set 0: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := d.ClaimTask(dispatchPending(t, d, p, "free"), a, p); err != nil {
			t.Fatalf("limit 0 (unlimited): claim %d: %v", i, err)
		}
	}
}

// AC6: the task's dispatcher (or an override actor) may force past the limit;
// the force is written to the task history. A plain doer's force is refused.
func TestWIPForceOverrideAudited(t *testing.T) {
	d := testDB(t)
	const p = "p1"
	regAgent(t, d, p, "dispatcher")
	dispatchClaimed(t, d, p, "dispatcher") // dispatcher holds one itself
	t2 := dispatchPending(t, d, p, "forced")
	if _, err := d.ClaimTaskForce(t2, "dispatcher", p); err != nil {
		t.Fatalf("dispatcher force claim: %v", err)
	}
	entries, err := d.ListAudit(p, t2, 20)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "wip_override" && e.Actor == "dispatcher" {
			found = true
		}
	}
	if !found {
		t.Fatalf("force past the WIP limit must write wip_override to the task history: %+v", entries)
	}

	const a = "worker-a"
	held := dispatchClaimed(t, d, p, a)
	other, err := d.DispatchTask(p, "", "someone-else", "not mine", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	_, err = d.ClaimTaskForce(other.ID, a, p)
	wantWIP(t, err, held, "doer force without override rights")
}

// AC7 (data side): the report lists every agent over its project's limit
// with the tasks it holds; agents within the limit are absent.
func TestWIPOverLimitReport(t *testing.T) {
	d := testDB(t)
	const p = "p1"
	regAgent(t, d, p, "worker-a")
	if err := d.SetProjectWIPLimit(p, 0); err != nil {
		t.Fatalf("set 0: %v", err)
	}
	h1 := dispatchClaimed(t, d, p, "worker-a")
	h2 := dispatchClaimed(t, d, p, "worker-a")
	dispatchClaimed(t, d, p, "worker-b")
	if err := d.SetProjectWIPLimit(p, 1); err != nil {
		t.Fatalf("set 1: %v", err)
	}
	rep, err := d.WIPOverLimit(p)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep) != 1 || rep[0].Agent != "worker-a" || rep[0].Limit != 1 || len(rep[0].Held) != 2 {
		t.Fatalf("report: want only worker-a holding 2 over limit 1, got %+v", rep)
	}
	ids := rep[0].Held[0].ID + rep[0].Held[1].ID
	if !strings.Contains(ids, h1) || !strings.Contains(ids, h2) {
		t.Fatalf("report must list both held tasks: %+v", rep[0].Held)
	}
}

// unlimitedWIP lifts the WIP limit of projects for a fixture that models one
// agent holding several active tasks (the scenario under test is not WIP).
func unlimitedWIP(t *testing.T, d *DB, projects ...string) {
	t.Helper()
	for _, p := range projects {
		d.EnsureProject(p)
		if err := d.SetProjectWIPLimit(p, 0); err != nil {
			t.Fatalf("unlimited wip %s: %v", p, err)
		}
	}
}

// A pending task queued to the worker (dispatch with assigned_to) is still
// counted when the worker starts it: the queue is not a WIP bypass.
func TestWIPQueuedStartCounted(t *testing.T) {
	d := testDB(t)
	const p, a = "p1", "worker-a"
	held := dispatchClaimed(t, d, p, a)
	queued, err := d.DispatchTask(p, "", "dispatcher", "queued", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := d.ReassignTaskFields(queued.ID, p, "dispatcher", strptr(a), nil, false); err != nil {
		t.Fatalf("queue to worker: %v", err)
	}
	_, err = d.StartTask(queued.ID, a, p)
	wantWIP(t, err, held, "start a queued task while holding one")
}
