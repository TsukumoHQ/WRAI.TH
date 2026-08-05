package db

import "testing"

// The git zone (branch/worktree/target) must round-trip through the column
// list ↔ scanTask lockstep and survive the in-review transition — it is what
// an external review gate consumes off the task.
func TestTaskGitZoneRoundTrip(t *testing.T) {
	d := testDB(t)

	task, err := d.DispatchTask("proj", "backend", "cto", "add rate limiting", "", "P1", nil, nil, TypedTicket{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if task.GitBranch != nil || task.GitWorktree != nil || task.GitTarget != nil {
		t.Fatalf("fresh task must have an empty git zone")
	}

	if _, err := d.ClaimTask(task.ID, "backend-1", "proj"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := d.StartTask(task.ID, "backend-1", "proj"); err != nil {
		t.Fatalf("start: %v", err)
	}

	branch := "feat/rate-limit"
	worktree := "/tmp/wt/rate-limit"
	target := "main"
	if err := d.SetTaskGit(task.ID, "proj", &branch, &worktree, &target); err != nil {
		t.Fatalf("set git: %v", err)
	}
	reviewed, err := d.ReviewTask(task.ID, "backend-1", "proj")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if reviewed.GitBranch == nil || *reviewed.GitBranch != branch {
		t.Fatalf("git_branch lost through in-review: %+v", reviewed.GitBranch)
	}
	if reviewed.GitWorktree == nil || *reviewed.GitWorktree != worktree {
		t.Fatalf("git_worktree lost: %+v", reviewed.GitWorktree)
	}
	if reviewed.GitTarget == nil || *reviewed.GitTarget != target {
		t.Fatalf("git_target lost: %+v", reviewed.GitTarget)
	}

	// Partial update must not wipe the other columns (COALESCE semantics).
	branch2 := "feat/rate-limit-v2"
	if err := d.SetTaskGit(task.ID, "proj", &branch2, nil, nil); err != nil {
		t.Fatalf("partial set: %v", err)
	}
	got, err := d.GetTask(task.ID, "proj")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GitBranch == nil || *got.GitBranch != branch2 {
		t.Fatalf("branch not updated: %+v", got.GitBranch)
	}
	if got.GitWorktree == nil || *got.GitWorktree != worktree {
		t.Fatalf("partial update wiped worktree: %+v", got.GitWorktree)
	}
}
