package relay

import (
	"testing"

	"agent-relay/internal/db"
)

// The G3 relay sweep converges a stranded merged PR task to done, applying the
// SAME prTargetFromState map the webhook/poll paths use. Exercises the relay
// wiring on top of db.ListStrandedPRTasks + db.ForcePRTransition.
func TestSweepStrandedPRTasks_Relay(t *testing.T) {
	r := testRelay(t)

	task, err := r.DB.DispatchTask("p1", "dev", "cto", "merged pr", "", "P2", nil, nil, db.TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Drive it to in-review, then record the PR as merged (webhook set pr_state
	// but the task transition to done was missed → stranded).
	if _, err := r.DB.ClaimTask(task.ID, "doer", "p1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := r.DB.StartTask(task.ID, "doer", "p1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := r.DB.ReviewTask(task.ID, "doer", "p1"); err != nil {
		t.Fatalf("review: %v", err)
	}
	url, repo, state := "https://x/pr/1", "org/repo", "merged"
	num := 1
	if _, err := r.DB.SetTaskPR(task.ID, "p1", &url, &num, &state, &repo); err != nil {
		t.Fatalf("set pr: %v", err)
	}

	r.sweepStrandedPRTasks()

	got, err := r.DB.GetTask(task.ID, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("stranded merged task should be swept to done, got %q", got.Status)
	}

	// Idempotent: a second sweep does nothing (already terminal).
	r.sweepStrandedPRTasks()
	if got2, _ := r.DB.GetTask(task.ID, "p1"); got2.Status != "done" {
		t.Fatalf("second sweep must not move a terminal task, got %q", got2.Status)
	}
}
