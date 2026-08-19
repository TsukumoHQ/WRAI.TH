package relay

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// reconcileResult calls reconcile_pr and returns the decoded {task, changed}.
func reconcileResult(t *testing.T, h *Handlers, args map[string]any) map[string]any {
	t.Helper()
	res, _ := h.HandleReconcilePr(ctx, call(args))
	if res.IsError {
		t.Fatalf("reconcile_pr error: %s", expectError(t, res))
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &body)
	return body
}

// The poll convergence tool applies the webhook status-map: a task with an open
// PR, reconciled as merged, transitions to done (changed=true), and a second
// identical reconcile is an idempotent no-op (changed=false).
func TestReconcilePrConvergesAndIsIdempotent(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)
	linkResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id,
		"pr_number": 9, "pr_repo": "o/r", "pr_state": "open",
	})

	body := reconcileResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "merged",
	})
	if body["changed"] != true {
		t.Fatalf("merged reconcile should transition, got changed=%v", body["changed"])
	}
	task, _ := body["task"].(map[string]any)
	if task["status"] != "done" || task["pr_state"] != "merged" {
		t.Fatalf("expected done+merged, got %v", task)
	}

	// Idempotent: same observation again does nothing.
	body2 := reconcileResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "merged",
	})
	if body2["changed"] != false {
		t.Fatalf("second identical reconcile must be a no-op, got changed=%v", body2["changed"])
	}
}

// closed-unmerged drives the task to blocked with the standard reason.
func TestReconcilePrClosedUnmergedBlocks(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)
	linkResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_number": 3, "pr_state": "open",
	})
	body := reconcileResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "closed",
	})
	task, _ := body["task"].(map[string]any)
	if task["status"] != "blocked" {
		t.Fatalf("closed-unmerged should block, got %v", task["status"])
	}
}

// No-resurrect: a terminal (done) task is never pulled back by a late/duplicate
// open observation.
func TestReconcilePrNoResurrect(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)
	linkResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_number": 5, "pr_state": "open",
	})
	// Drive to done.
	reconcileResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "merged",
	})
	// A stale "open" must NOT resurrect it.
	body := reconcileResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "open",
	})
	if body["changed"] != false {
		t.Fatalf("stale open must not resurrect a done task, got changed=%v", body["changed"])
	}
	task, _ := body["task"].(map[string]any)
	if task["status"] != "done" {
		t.Fatalf("done task must stay done, got %v", task["status"])
	}
}

func TestReconcilePrValidation(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)
	// Missing / bad pr_state.
	res, _ := h.HandleReconcilePr(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "bogus",
	}))
	if !res.IsError {
		t.Fatal("expected validation error for bad pr_state")
	}
	// Missing task.
	registerActive(t, h, "p1", "bot-a", nil)
	res2, _ := h.HandleReconcilePr(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": "00000000-0000-0000-0000-000000000000", "pr_state": "open",
	}))
	if !res2.IsError {
		t.Fatal("expected NOT_FOUND for missing task")
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res2.Content[0].(mcp.TextContent).Text), &body)
	if body["code"] != CodeNotFound {
		t.Fatalf("expected NOT_FOUND: %v", body)
	}
}

// prTargetFromState is the state-keyed twin of prTargetState — same map.
func TestPrTargetFromState(t *testing.T) {
	cases := map[string]string{"open": "in-review", "merged": "done", "closed": "blocked", "weird": ""}
	for in, want := range cases {
		if got, _ := prTargetFromState(in); got != want {
			t.Fatalf("prTargetFromState(%q) = %q, want %q", in, got, want)
		}
	}
}
