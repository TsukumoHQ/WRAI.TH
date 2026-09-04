package relay

import (
	"strings"
	"testing"
)

// dispatchAndClaim dispatches a task from cto to profile "dev" and claims it as
// holder, returning the task id. Shared setup for the reassign tests.
func dispatchClaimedTask(t *testing.T, h *Handlers, holder string) string {
	t.Helper()
	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "reassign me",
	}))
	if dispatchRes.IsError {
		t.Fatalf("dispatch: %s", expectError(t, dispatchRes))
	}
	taskID := parseJSON(t, dispatchRes)["task"].(map[string]any)["id"].(string)
	claimRes, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": holder, "task_id": taskID}))
	if claimRes.IsError {
		t.Fatalf("claim: %s", expectError(t, claimRes))
	}
	return taskID
}

// TestUpdateTask_ReassignClaimed_TransfersLease covers DEC rule (a): assigned_to
// on a CLAIMED task atomically moves the lease to the new agent — claimed_by /
// lease_holder repoint, status is unchanged, profile_slug follows the new
// assignee, and a lease_transfer{reason:"reassigned",by:caller} marker rides the
// response. (AC1)
func TestUpdateTask_ReassignClaimed_TransfersLease(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-b", "role": "dev", "profile_slug": "backend"}))

	taskID := dispatchClaimedTask(t, h, "dev-a")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID, "assigned_to": "dev-b",
	}))
	if res.IsError {
		t.Fatalf("dispatcher reassign of a claimed task should succeed: %s", expectError(t, res))
	}
	got := parseJSON(t, res)
	if got["assigned_to"] != "dev-b" {
		t.Errorf("assigned_to = %v, want dev-b", got["assigned_to"])
	}
	if got["claimed_by"] != "dev-b" {
		t.Errorf("claimed_by = %v, want dev-b (lease transferred)", got["claimed_by"])
	}
	if got["lease_holder"] != "dev-b" {
		t.Errorf("lease_holder = %v, want dev-b", got["lease_holder"])
	}
	if got["status"] != "accepted" {
		t.Errorf("status = %v, want accepted (unchanged by a reassign)", got["status"])
	}
	if got["profile_slug"] != "backend" {
		t.Errorf("profile_slug = %v, want backend (recomputed from new assignee)", got["profile_slug"])
	}
	lt, ok := got["lease_transfer"].(map[string]any)
	if !ok {
		t.Fatalf("lease_transfer marker missing on the response: %+v", got)
	}
	if lt["from"] != "dev-a" || lt["to"] != "dev-b" || lt["reason"] != "reassigned" || lt["by"] != "cto" {
		t.Errorf("lease_transfer = %+v, want {from:dev-a,to:dev-b,reason:reassigned,by:cto}", lt)
	}

	// get_task must reflect the persisted transfer (lease_transfer itself is
	// transient and not persisted).
	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	after := parseJSON(t, getRes)
	if after["claimed_by"] != "dev-b" || after["lease_holder"] != "dev-b" || after["status"] != "accepted" {
		t.Fatalf("persisted reassignment wrong: %+v", after)
	}
}

// TestUpdateTask_ReassignClaimed_ProfileSlugAloneRefused covers DEC rule (b): a
// profile_slug change alone on a claimed task is refused (pass assigned_to), and
// leaves the held task untouched. (AC1)
func TestUpdateTask_ReassignClaimed_ProfileSlugAloneRefused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))

	taskID := dispatchClaimedTask(t, h, "dev-a")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID, "profile_slug": "backend",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "claimed by dev-a") || !strings.Contains(msg, "assigned_to") {
		t.Errorf("refusal must name the holder and point to assigned_to, got: %s", msg)
	}
	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	after := parseJSON(t, getRes)
	if after["profile_slug"] != "dev" || after["claimed_by"] != "dev-a" {
		t.Fatalf("held task must be untouched after a refused profile-only reassign: %+v", after)
	}
}

// TestUpdateTask_ReassignPending_BothFieldsFree covers DEC rule (c): on a pending
// task both fields update freely and NO lease is minted (the task stays
// claimable). An explicit profile_slug wins over the recompute. (AC1)
func TestUpdateTask_ReassignPending_BothFieldsFree(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-b", "role": "dev", "profile_slug": "backend"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "pending reassign",
	}))
	taskID := parseJSON(t, dispatchRes)["task"].(map[string]any)["id"].(string)

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID,
		"assigned_to": "dev-b", "profile_slug": "custom-prof",
	}))
	if res.IsError {
		t.Fatalf("pending reassign should succeed: %s", expectError(t, res))
	}
	got := parseJSON(t, res)
	if got["assigned_to"] != "dev-b" {
		t.Errorf("assigned_to = %v, want dev-b", got["assigned_to"])
	}
	if got["profile_slug"] != "custom-prof" {
		t.Errorf("profile_slug = %v, want custom-prof (explicit wins over recompute)", got["profile_slug"])
	}
	if got["status"] != "pending" {
		t.Errorf("status = %v, want pending (unchanged)", got["status"])
	}
	if _, minted := got["lease_holder"]; minted {
		t.Errorf("a pending reassign must not mint a lease, got lease_holder=%v", got["lease_holder"])
	}
	if _, claimed := got["claimed_by"]; claimed {
		t.Errorf("a pending reassign must not set claimed_by, got %v", got["claimed_by"])
	}
}

// TestUpdateTask_ReassignDoerCannotReassignOwnTask covers DEC rule (d): a plain
// doer cannot reassign its own task — only the dispatcher, a lead in its chain,
// or an executive may. (AC1)
func TestUpdateTask_ReassignDoerCannotReassignOwnTask(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-b", "role": "dev"}))

	taskID := dispatchClaimedTask(t, h, "dev-a")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "dev-a", "task_id": taskID, "assigned_to": "dev-b",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "reassign") {
		t.Errorf("a doer reassigning its own task must be refused, got: %s", msg)
	}
	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	if parseJSON(t, getRes)["claimed_by"] != "dev-a" {
		t.Fatal("task must still be held by dev-a after a refused self-reassign")
	}
}

// TestUpdateTask_ReassignByLeadChain covers DEC rule (d): an agent in the doer's
// reports_to chain (a lead) may reassign even when it is not the dispatcher. (AC1)
func TestUpdateTask_ReassignByLeadChain(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "lead-x", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev", "reports_to": "lead-x"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-b", "role": "dev"}))

	taskID := dispatchClaimedTask(t, h, "dev-a")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "lead-x", "task_id": taskID, "assigned_to": "dev-b",
	}))
	if res.IsError {
		t.Fatalf("a lead in the doer's chain must be allowed to reassign: %s", expectError(t, res))
	}
	if parseJSON(t, res)["claimed_by"] != "dev-b" {
		t.Fatal("lead-chain reassign should transfer the lease to dev-b")
	}
}

// TestUpdateTask_ReassignByExecutive covers DEC rule (d): an executive may
// reassign any task. (AC1)
func TestUpdateTask_ReassignByExecutive(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "boss", "role": "exec", "is_executive": true}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-b", "role": "dev"}))

	taskID := dispatchClaimedTask(t, h, "dev-a")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "boss", "task_id": taskID, "assigned_to": "dev-b",
	}))
	if res.IsError {
		t.Fatalf("an executive must be allowed to reassign: %s", expectError(t, res))
	}
	if parseJSON(t, res)["claimed_by"] != "dev-b" {
		t.Fatal("executive reassign should transfer the lease to dev-b")
	}
}

// TestUpdateTask_UnknownFieldRefused covers AC2 / DEC rule (e): a field
// update_task does not recognise is refused loudly, naming it — never silently
// ignored with a 200 + unchanged record. A status is not a field: it has verbs.
func TestUpdateTask_UnknownFieldRefused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "no unknown fields",
	}))
	taskID := parseJSON(t, dispatchRes)["task"].(map[string]any)["id"].(string)

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID, "status": "done",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "status") || !strings.Contains(msg, "not an updatable field") {
		t.Errorf("unknown field must be refused by name, got: %s", msg)
	}
}
