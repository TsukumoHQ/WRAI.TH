package relay

import (
	"testing"

	"agent-relay/internal/db"
)

// W8 D3 (ruling wraith-deploying-ruling): on a pipeline repo the merge moves a
// task to deploying; done waits for the deploy + verify. A GitHub "merged"
// observation (webhook or reconcile_pr poll) arriving on a deploying task is
// already accounted for and must not close it early.

// toDeploying drives a task through claim/start/review as dev-a, then the
// pipeline (niwa) moves it to deploying.
func toDeploying(t *testing.T, d *db.DB, id string) {
	t.Helper()
	if _, err := d.ClaimTask(id, "dev-a", "p1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := d.StartTask(id, "dev-a", "p1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := d.ReviewTask(id, "dev-a", "p1"); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := d.DeployTask(id, "niwa", "p1", "4d01274a056a"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
}

// AC1: a merged webhook on a deploying task leaves it deploying.
func TestPRMergedWebhookLeavesDeploying(t *testing.T) {
	r := testRelay(t)
	id := seedLinkedTask(t, r, 42, "o/repo")
	toDeploying(t, r.DB, id)
	r.syncPullRequestToTask("p1", prPayload("closed", 42, "o/repo", true, ""))
	if s := statusOf(t, r, id); s != "deploying" {
		t.Fatalf("merged webhook moved a deploying task to %s, want deploying", s)
	}
	r.syncPullRequestToTask("p1", prPayload("reopened", 42, "o/repo", false, ""))
	if s := statusOf(t, r, id); s != "deploying" {
		t.Fatalf("a late reopened webhook moved a deploying task to %s, want deploying", s)
	}
}

// AC2: reconcile_pr(pr_state=merged) on a deploying task leaves it deploying
// and returns no error.
func TestReconcilePrMergedLeavesDeploying(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)
	linkResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_number": 9, "pr_repo": "o/r", "pr_state": "open",
	})
	toDeploying(t, h.db, id)
	body := reconcileResult(t, h, map[string]any{ // fails the test on an error result
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "merged",
	})
	if body["changed"] != false {
		t.Fatalf("reconcile merged on a deploying task changed=%v, want false", body["changed"])
	}
	task, _ := h.db.GetTask(id, "p1")
	if task == nil || task.Status != "deploying" {
		t.Fatalf("status after reconcile = %v, want deploying", task)
	}
}

// AC3: a merged webhook on an in-review task (non-pipeline repo) still moves
// it to done as today.
func TestPRMergedWebhookInReviewStillDone(t *testing.T) {
	r := testRelay(t)
	id := seedLinkedTask(t, r, 43, "o/repo")
	r.syncPullRequestToTask("p1", prPayload("opened", 43, "o/repo", false, ""))
	if s := statusOf(t, r, id); s != "in-review" {
		t.Fatalf("setup: opened -> %s, want in-review", s)
	}
	r.syncPullRequestToTask("p1", prPayload("closed", 43, "o/repo", true, ""))
	if s := statusOf(t, r, id); s != "done" {
		t.Fatalf("merged webhook on in-review -> %s, want done", s)
	}
}
