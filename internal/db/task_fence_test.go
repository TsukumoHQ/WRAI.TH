package db

import (
	"errors"
	"strings"
	"testing"
)

// S3 0b980988 — lease fencing on the terminal writes (complete / block /
// review): only the current lease holder, at the current lease generation, or
// an explicit override actor (dispatcher / human / RELAY_OVERRIDE_ACTORS) may
// publish. Everyone else gets TASK_LEASE_FENCED and the row is untouched.

func wantFenced(t *testing.T, err error, what string) {
	t.Helper()
	var te *TaskError
	if !errors.As(err, &te) || te.Code != CodeTaskLeaseFenced {
		t.Fatalf("%s: want %s, got %v", what, CodeTaskLeaseFenced, err)
	}
}

func gen(n int64) *int64 { return &n }

func mustStatus(t *testing.T, d *DB, id, project, want string) {
	t.Helper()
	task, err := d.GetTask(id, project)
	if err != nil || task == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if task.Status != want {
		t.Fatalf("status: want %q, got %q", want, task.Status)
	}
}

// TestFenceNonHolderTerminalWritesRefused: a registered agent that is not the
// holder cannot complete, block or review the holder's task.
func TestFenceNonHolderTerminalWritesRefused(t *testing.T) {
	d := testDB(t)
	const project, holder, other = "p1", "worker-a", "worker-b"
	id := dispatchClaimed(t, d, project, holder)
	regAgent(t, d, project, other)
	if _, err := d.StartTask(id, holder, project); err != nil {
		t.Fatalf("start: %v", err)
	}

	_, err := d.CompleteTaskFenced(id, other, project, nil, nil)
	wantFenced(t, err, "complete by non-holder")
	_, err = d.BlockTaskFenced(id, other, project, nil, "", nil)
	wantFenced(t, err, "block by non-holder")
	_, err = d.ReviewTaskFenced(id, other, project, nil)
	wantFenced(t, err, "review by non-holder")
	// The unfenced wrappers route through the same check.
	_, err = d.CompleteTask(id, other, project, nil)
	wantFenced(t, err, "CompleteTask by non-holder")
	mustStatus(t, d, id, project, "in-progress")

	if _, err := d.ReviewTaskFenced(id, holder, project, nil); err != nil {
		t.Fatalf("review by holder: %v", err)
	}
	if _, err := d.CompleteTaskFenced(id, holder, project, nil, nil); err != nil {
		t.Fatalf("complete by holder: %v", err)
	}
	mustStatus(t, d, id, project, "done")
}

// TestFenceUnheldPendingDoneRefused: pending→done is a legal edge, but a
// pending task has no holder, so a worker cannot publish it directly.
func TestFenceUnheldPendingDoneRefused(t *testing.T) {
	d := testDB(t)
	const project, worker = "p1", "worker-a"
	regAgent(t, d, project, worker)
	task, err := d.DispatchTask(project, "", "dispatcher", "unheld", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	_, err = d.CompleteTask(task.ID, worker, project, nil)
	wantFenced(t, err, "pending→done by non-holder")
	mustStatus(t, d, task.ID, project, "pending")
}

// TestFenceStaleGenerationRefused: a same-name worker whose lease was re-granted
// (reclaim bumps the generation) cannot publish with the old generation.
func TestFenceStaleGenerationRefused(t *testing.T) {
	d := testDB(t)
	const project, holder = "p1", "worker-a"
	id := dispatchClaimed(t, d, project, holder)
	first, _ := d.GetTask(id, project)
	stale := first.LeaseGeneration

	forceLeaseExpired(t, d, id, project)
	re, err := d.ReclaimTask(id, holder, project)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if re.LeaseGeneration != stale+1 {
		t.Fatalf("reclaim generation: want %d, got %d", stale+1, re.LeaseGeneration)
	}
	if _, err := d.StartTask(id, holder, project); err != nil {
		t.Fatalf("start: %v", err)
	}

	_, err = d.CompleteTaskFenced(id, holder, project, nil, gen(stale))
	wantFenced(t, err, "complete with stale generation")
	_, err = d.BlockTaskFenced(id, holder, project, nil, "", gen(stale))
	wantFenced(t, err, "block with stale generation")
	_, err = d.ReviewTaskFenced(id, holder, project, gen(stale))
	wantFenced(t, err, "review with stale generation")
	mustStatus(t, d, id, project, "in-progress")

	if _, err := d.CompleteTaskFenced(id, holder, project, nil, gen(re.LeaseGeneration)); err != nil {
		t.Fatalf("complete with current generation: %v", err)
	}
}

// TestFenceStrictRequiresGeneration: RELAY_STRICT_FENCING=1 refuses a holder
// write that omits the generation.
func TestFenceStrictRequiresGeneration(t *testing.T) {
	t.Setenv("RELAY_STRICT_FENCING", "1")
	d := testDB(t)
	const project, holder = "p1", "worker-a"
	id := dispatchClaimed(t, d, project, holder)
	_, err := d.CompleteTaskFenced(id, holder, project, nil, nil)
	wantFenced(t, err, "strict: complete without generation")
	task, _ := d.GetTask(id, project)
	if _, err := d.CompleteTaskFenced(id, holder, project, nil, gen(task.LeaseGeneration)); err != nil {
		t.Fatalf("strict: complete with generation: %v", err)
	}
}

// TestLeaseGenerationBumpsOnEveryGrant: claim, reclaim, reassign (lease moves)
// and update_task transfer each bump the generation by one; claim_next too.
func TestLeaseGenerationBumpsOnEveryGrant(t *testing.T) {
	d := testDB(t)
	const project, a, b, c = "p1", "worker-a", "worker-b", "worker-c"
	id := dispatchClaimed(t, d, project, a)
	regAgent(t, d, project, b)
	regAgent(t, d, project, c)
	g := func() int64 {
		task, _ := d.GetTask(id, project)
		return task.LeaseGeneration
	}
	if g() != 1 {
		t.Fatalf("after claim: want 1, got %d", g())
	}
	forceLeaseExpired(t, d, id, project)
	if _, err := d.ReclaimTask(id, b, project); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if g() != 2 {
		t.Fatalf("after reclaim: want 2, got %d", g())
	}
	if _, err := d.ReassignTaskFields(id, project, "dispatcher", strptr(c), nil, true); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if g() != 3 {
		t.Fatalf("after transfer: want 3, got %d", g())
	}
	if _, err := d.ReassignTask(id, project, a); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if g() != 4 {
		t.Fatalf("after reassign: want 4, got %d", g())
	}
	// The replaced workers are fenced even without echoing a generation.
	_, err := d.CompleteTask(id, b, project, nil)
	wantFenced(t, err, "complete by transferred-away worker")

	next, err := d.DispatchTask(project, "", "dispatcher", "next", "", "P1", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	claimed, err := d.ClaimNextTask(project, b, "", "", SortPriority)
	if err != nil || claimed == nil || claimed.ID != next.ID {
		t.Fatalf("claim_next: %v %v", claimed, err)
	}
	if claimed.LeaseGeneration != 1 {
		t.Fatalf("claim_next generation: want 1, got %d", claimed.LeaseGeneration)
	}
}

// TestFenceOverrideAudited: the dispatcher, the human operator and the
// RELAY_OVERRIDE_ACTORS allowlist (default "niwa") may publish over the holder;
// each such write leaves a lease_override row in the task's audit history. An
// is_service agent outside the allowlist is fenced like any other.
func TestFenceOverrideAudited(t *testing.T) {
	d := testDB(t)
	const project, holder = "p1", "worker-a"
	if _, _, err := d.RegisterAgent(project, "svc", "test", "", nil, nil, false, nil, "[]", 0,
		RegisterOptions{IsService: true, IsServiceSet: true}); err != nil {
		t.Fatalf("register svc: %v", err)
	}

	overrides := func(id string) int {
		entries, err := d.ListAudit(project, id, 50)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		n := 0
		for _, e := range entries {
			if e.Action == "lease_override" {
				n++
			}
		}
		return n
	}

	for _, actor := range []string{"dispatcher", "human", "niwa"} {
		id := dispatchClaimed(t, d, project, holder)
		_, err := d.CompleteTask(id, "svc", project, nil)
		wantFenced(t, err, "is_service agent outside allowlist")
		if _, err := d.CompleteTask(id, actor, project, nil); err != nil {
			t.Fatalf("override complete by %s: %v", actor, err)
		}
		mustStatus(t, d, id, project, "done")
		if n := overrides(id); n != 1 {
			t.Fatalf("override by %s: want 1 lease_override audit row, got %d", actor, n)
		}
	}

	// The allowlist is configurable: svc listed passes, niwa unlisted is fenced.
	t.Setenv("RELAY_OVERRIDE_ACTORS", "svc")
	id := dispatchClaimed(t, d, project, holder)
	_, err := d.CompleteTask(id, "niwa", project, nil)
	wantFenced(t, err, "niwa removed from allowlist")
	if _, err := d.CompleteTask(id, "svc", project, nil); err != nil {
		t.Fatalf("override by listed svc: %v", err)
	}

	// A holder publishing its own task is not an override.
	id = dispatchClaimed(t, d, project, holder)
	if _, err := d.CompleteTask(id, holder, project, nil); err != nil {
		t.Fatalf("holder complete: %v", err)
	}
	if n := overrides(id); n != 0 {
		t.Fatalf("holder complete: want 0 lease_override rows, got %d", n)
	}
}

// TestOverrideResumeKeepsHolder: the gate daemon blocks (escalation) and later
// resumes a worker's task; the resume must hand the lease back to the worker,
// not to the daemon, so the worker's resubmit is accepted.
func TestOverrideResumeKeepsHolder(t *testing.T) {
	d := testDB(t)
	const project, holder = "p1", "worker-a"
	id := dispatchClaimed(t, d, project, holder)
	if _, err := d.StartTask(id, holder, project); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := d.ReviewTask(id, holder, project); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := d.BlockTask(id, "niwa", project, strptr("gate rejected")); err != nil {
		t.Fatalf("niwa block: %v", err)
	}
	resumed, err := d.StartTask(id, "niwa", project)
	if err != nil {
		t.Fatalf("niwa resume: %v", err)
	}
	if got := strVal(resumed.LeaseHolder); got != holder {
		t.Fatalf("resume lease_holder: want %q, got %q", holder, got)
	}
	if got := strVal(resumed.AssignedTo); got != holder {
		t.Fatalf("resume assigned_to: want %q, got %q", holder, got)
	}
	if _, err := d.ReviewTaskFenced(id, holder, project, gen(resumed.LeaseGeneration)); err != nil {
		t.Fatalf("worker resubmit after resume: %v", err)
	}
}

// TestFenceBlockedPointsToResume: a block releases the lease, so a worker
// publishing straight from blocked is fenced and told to resume_task; after
// resuming it holds the lease again and publishes.
func TestFenceBlockedPointsToResume(t *testing.T) {
	d := testDB(t)
	const project, holder = "p1", "worker-a"
	id := dispatchClaimed(t, d, project, holder)
	if _, err := d.StartTask(id, holder, project); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := d.BlockTask(id, holder, project, strptr("waiting")); err != nil {
		t.Fatalf("block: %v", err)
	}
	_, err := d.ReviewTask(id, holder, project)
	wantFenced(t, err, "review straight from blocked")
	if !strings.Contains(err.Error(), "resume_task") {
		t.Fatalf("fenced-from-blocked message should point to resume_task: %v", err)
	}
	if _, err := d.StartTask(id, holder, project); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := d.ReviewTask(id, holder, project); err != nil {
		t.Fatalf("review after resume: %v", err)
	}
}
