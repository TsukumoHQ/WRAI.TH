package relay

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"agent-relay/internal/models"
)

// N3 relay half (03958111): a lifecycle call by the delegating service 'niwa'
// (RELAY_OVERRIDE_ACTORS, IsDelegatingService) acts FOR the doer: lease_holder
// and assigned_to never move to the daemon identity.

func n3Fixture(t *testing.T) (*Handlers, string) {
	t.Helper()
	h := testHandlers(t)
	for _, name := range []string{"cto", "dev-a", "niwa", "peer"} {
		_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": name}))
	}
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "profile": "dev", "title": "n3 work"}))
	id := parseJSON(t, res)["task"].(map[string]any)["id"].(string)
	for _, step := range []string{"claim", "start"} {
		var r = map[string]any{"project": "p1", "as": "dev-a", "task_id": id}
		if step == "claim" {
			res, _ = h.HandleClaimTask(ctx, call(r))
		} else {
			res, _ = h.HandleStartTask(ctx, call(r))
		}
		if res.IsError {
			t.Fatalf("%s by doer: %s", step, expectError(t, res))
		}
	}
	return h, id
}

func n3Task(t *testing.T, h *Handlers, id string) *models.Task {
	t.Helper()
	task, err := h.db.GetTask(id, "p1")
	if err != nil || task == nil {
		t.Fatalf("get task: %v", err)
	}
	return task
}

func strOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// assertDoerKeeps checks the doer still owns the task: assigned_to is dev-a and
// the lease is dev-a's, or released by a block / completion — never the
// caller's.
func assertDoerKeeps(t *testing.T, task *models.Task, step string) {
	t.Helper()
	if got := strOf(task.AssignedTo); got != "dev-a" {
		t.Errorf("%s: assigned_to = %q, want dev-a", step, got)
	}
	released := task.Status == "blocked" || task.Status == "done"
	if got := strOf(task.LeaseHolder); got != "dev-a" && !(released && got == "") {
		t.Errorf("%s: lease_holder = %q (status %s), want dev-a", step, got, task.Status)
	}
}

func n3Handler(h *Handlers, tool string) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"review_task": h.HandleReviewTask, "start_task": h.HandleStartTask,
		"block_task": h.HandleBlockTask, "resume_task": h.HandleResumeTask,
		"complete_task": h.HandleCompleteTask,
	}[tool]
}

// n3Call runs tool as caller on task id and asserts it succeeded and the doer
// kept the task.
func n3Call(t *testing.T, h *Handlers, id, caller, tool string) {
	t.Helper()
	res, _ := n3Handler(h, tool)(ctx, call(map[string]any{"project": "p1", "as": caller, "task_id": id, "reason": "gate", "result": "merged"}))
	if res.IsError {
		t.Fatalf("%s by %s: %s", tool, caller, expectError(t, res))
	}
	assertDoerKeeps(t, n3Task(t, h, id), tool+" by "+caller)
}

// Ruling (cto-tsukumo 16:40Z): start / review / block / resume / complete never
// move lease_holder or assigned_to, whoever calls with authority — the listed
// delegating service, the dispatcher, the human operator.
func TestDelegatedLifecycle_DoerKeepsLeaseAndAssignment(t *testing.T) {
	for _, caller := range []string{"niwa", "cto", "user"} {
		t.Run(caller, func(t *testing.T) {
			h, id := n3Fixture(t)
			for _, tool := range []string{"review_task", "start_task", "block_task", "resume_task", "block_task", "review_task", "complete_task"} {
				n3Call(t, h, id, caller, tool)
			}
		})
	}
}

// The field case: the daemon is NOT a listed delegating service (tokenless,
// RELAY_OVERRIDE_ACTORS without it). Its unfenced moves — start after a
// review, resume after a block — still leave the task with the doer.
func TestDelegatedLifecycle_UnlistedCallerNeverTakesTheTask(t *testing.T) {
	t.Setenv("RELAY_OVERRIDE_ACTORS", "someone-else")
	h, id := n3Fixture(t)
	n3Call(t, h, id, "dev-a", "review_task")
	n3Call(t, h, id, "niwa", "start_task") // in-review → in-progress (gate reject)
	n3Call(t, h, id, "dev-a", "block_task")
	n3Call(t, h, id, "niwa", "resume_task") // blocked → in-progress
}

// Only claim / reclaim / update_task assigned_to move a lease, each with its
// existing authority check.
func TestLeaseMovesOnlyOnClaimReclaimReassign(t *testing.T) {
	h, id := n3Fixture(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-b"}))
	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": id, "assigned_to": "dev-b"}))
	if res.IsError {
		t.Fatalf("dispatcher update_task assigned_to: %s", expectError(t, res))
	}
	if task := n3Task(t, h, id); strOf(task.AssignedTo) != "dev-b" || strOf(task.LeaseHolder) != "dev-b" {
		t.Fatalf("update_task assigned_to: assigned %q lease %q, want dev-b", strOf(task.AssignedTo), strOf(task.LeaseHolder))
	}
	// dev-b goes dead: peer reclaims (existing rule: holder inactive).
	if err := h.db.DeactivateAgent("p1", "dev-b"); err != nil {
		t.Fatal(err)
	}
	res, _ = h.HandleReclaimTask(ctx, call(map[string]any{"project": "p1", "as": "peer", "task_id": id}))
	if res.IsError {
		t.Fatalf("reclaim from a dead holder: %s", expectError(t, res))
	}
	if task := n3Task(t, h, id); strOf(task.LeaseHolder) != "peer" {
		t.Fatalf("reclaim: lease %q, want peer", strOf(task.LeaseHolder))
	}
	res, _ = h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "profile": "dev", "title": "fresh"}))
	fresh := parseJSON(t, res)["task"].(map[string]any)["id"].(string)
	res, _ = h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "dev-b2", "task_id": fresh}))
	if res.IsError {
		t.Fatalf("claim: %s", expectError(t, res))
	}
	if task := n3Task(t, h, fresh); strOf(task.LeaseHolder) != "dev-b2" {
		t.Fatalf("claim: lease %q, want dev-b2", strOf(task.LeaseHolder))
	}
}

// A non-delegating agent acting on someone else's held task keeps today's
// rule: the fenced review is refused and nothing moves.
func TestDelegatedLifecycle_PeerReviewRefused(t *testing.T) {
	h, id := n3Fixture(t)
	res, _ := h.HandleReviewTask(ctx, call(map[string]any{"project": "p1", "as": "peer", "task_id": id}))
	if !res.IsError {
		t.Fatal("peer review_task on dev-a's task accepted, want refused")
	}
	task := n3Task(t, h, id)
	if task.Status != "in-progress" || strOf(task.LeaseHolder) != "dev-a" || strOf(task.AssignedTo) != "dev-a" {
		t.Errorf("after refused peer review: status %s lease %q assigned %q, want in-progress/dev-a/dev-a", task.Status, strOf(task.LeaseHolder), strOf(task.AssignedTo))
	}
}

// is_service is self-settable, so it is never the trust root: the act-for-the-
// doer path keys on RELAY_OVERRIDE_ACTORS only. Same task shape, same calls:
// 'niwa' (listed, registered WITHOUT is_service) acts for the doer; 'svc'
// (not listed, registered WITH is_service=true) gets the plain fence — its
// review is refused and its start never hands it the task.
func TestDelegatedLifecycle_SelfSetServiceGetsNoDelegation(t *testing.T) {
	t.Setenv("RELAY_OVERRIDE_ACTORS", "niwa")
	h, id := n3Fixture(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "svc", "is_service": true}))
	if ag, _ := h.db.GetAgent("p1", "svc"); ag == nil || !ag.IsService {
		t.Fatal("precondition: svc must carry is_service=true")
	}
	if ag, _ := h.db.GetAgent("p1", "niwa"); ag == nil || ag.IsService {
		t.Fatal("precondition: niwa must NOT carry is_service (the env list alone delegates)")
	}

	res, _ := h.HandleReviewTask(ctx, call(map[string]any{"project": "p1", "as": "svc", "task_id": id}))
	if !res.IsError {
		t.Fatal("is_service=true agent review_task on dev-a's task accepted, want refused")
	}
	res, _ = h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "svc", "task_id": id}))
	task := n3Task(t, h, id)
	if strOf(task.AssignedTo) == "svc" || strOf(task.LeaseHolder) == "svc" {
		t.Errorf("is_service=true agent took the task: assigned %q lease %q", strOf(task.AssignedTo), strOf(task.LeaseHolder))
	}

	res, _ = h.HandleReviewTask(ctx, call(map[string]any{"project": "p1", "as": "niwa", "task_id": id}))
	if res.IsError {
		t.Fatalf("listed delegating service review_task refused: %s", expectError(t, res))
	}
	assertDoerKeeps(t, n3Task(t, h, id), "review_task")
}

// With no doer on record (never claimed), the delegating service has nobody to
// act for: the call is refused instead of handing the task to 'niwa'.
func TestDelegatedLifecycle_NoDoerRefusedNeverAssignedToService(t *testing.T) {
	h := testHandlers(t)
	for _, name := range []string{"cto", "niwa"} {
		_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": name}))
	}
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "profile": "dev", "title": "unclaimed"}))
	id := parseJSON(t, res)["task"].(map[string]any)["id"].(string)
	res, _ = h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "niwa", "task_id": id}))
	if !res.IsError {
		t.Error("niwa start_task on an unclaimed task accepted, want refused (no doer to act for)")
	}
	task := n3Task(t, h, id)
	if strOf(task.AssignedTo) == "niwa" || strOf(task.LeaseHolder) == "niwa" {
		t.Errorf("task handed to the delegating service: assigned %q lease %q", strOf(task.AssignedTo), strOf(task.LeaseHolder))
	}
}
