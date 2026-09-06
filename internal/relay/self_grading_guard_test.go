package relay

import (
	"strings"
	"testing"
)

// selfDispatch registers `doer` (optionally under `lead`), has doer dispatch a
// task to its own profile and then claim it, so the row ends up with
// DispatchedBy == AssignedTo == doer — the self-dispatched shape from finding
// bf920f6c. Returns the task id.
func selfDispatch(t *testing.T, h *Handlers, project, doer, lead string) string {
	t.Helper()
	reg := map[string]any{"project": project, "name": doer, "role": "dev"}
	if lead != "" {
		reg["reports_to"] = lead
	}
	if _, err := h.HandleRegisterAgent(ctx, call(reg)); err != nil {
		t.Fatalf("register %s: %v", doer, err)
	}
	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": project, "as": doer, "profile": "dev", "title": "self-graded work",
		"goal": "orig goal", "acceptance_criteria": `["ac1","ac2","ac3","ac4","ac5"]`, "dod": "orig dod",
	}))
	if dispatchRes.IsError {
		t.Fatalf("self-dispatch should succeed: %s", expectError(t, dispatchRes))
	}
	taskID := parseJSON(t, dispatchRes)["task"].(map[string]any)["id"].(string)
	claimRes, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": project, "as": doer, "task_id": taskID}))
	if claimRes.IsError {
		t.Fatalf("doer claiming its own task should succeed: %s", expectError(t, claimRes))
	}
	return taskID
}

// TestUpdateTask_SelfDispatchedDoerCannotRescopeOwnContract is the bf920f6c
// repro (AC1): on a self-dispatched task the doer is also the dispatcher, so the
// dispatcher check alone would wave the doer straight through to rewrite its own
// acceptance_criteria (5→2 in the live case). The self-grading guard must refuse
// loudly and leave the contract untouched.
func TestUpdateTask_SelfDispatchedDoerCannotRescopeOwnContract(t *testing.T) {
	h := testHandlers(t)
	taskID := selfDispatch(t, h, "p1", "solo-dev", "")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "solo-dev", "task_id": taskID,
		"acceptance_criteria": `["ac1","ac2"]`,
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "self-dispatched") || !strings.Contains(msg, "DEC-wraith-self-grading-guard-1") {
		t.Errorf("refusal must name the self-grading rule + DEC, got: %s", msg)
	}

	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	if got := parseJSON(t, getRes); got["acceptance_criteria"] != `["ac1","ac2","ac3","ac4","ac5"]` {
		t.Fatalf("contract must be UNCHANGED after a refused self-edit, got: %+v", got["acceptance_criteria"])
	}
}

// TestUpdateTask_SelfDispatchedContractNeedsSignoffFromAbove is AC2: the same
// contract edit succeeds when performed by a sign-off authority ABOVE the doer —
// an agent in the doer's reports_to lead chain, or an executive — never the doer
// itself.
func TestUpdateTask_SelfDispatchedContractNeedsSignoffFromAbove(t *testing.T) {
	h := testHandlers(t)
	if _, err := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "lead", "role": "lead"})); err != nil {
		t.Fatalf("register lead: %v", err)
	}
	taskID := selfDispatch(t, h, "p1", "solo-dev", "lead")

	// The doer's reports_to lead may sign off the new contract.
	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "lead", "task_id": taskID,
		"goal": "signed goal", "acceptance_criteria": `["ac1","ac2"]`, "dod": "signed dod",
	}))
	if res.IsError {
		t.Fatalf("the doer's lead should be able to sign off the contract: %s", expectError(t, res))
	}
	updated := parseJSON(t, res)
	if updated["goal"] != "signed goal" || updated["acceptance_criteria"] != `["ac1","ac2"]` || updated["dod"] != "signed dod" {
		t.Fatalf("lead's contract sign-off did not land: %+v", updated)
	}

	// An executive is a superset sign-off authority.
	if _, err := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "chief", "role": "exec", "is_executive": true})); err != nil {
		t.Fatalf("register exec: %v", err)
	}
	exRes, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "chief", "task_id": taskID, "dod": "exec dod",
	}))
	if exRes.IsError {
		t.Fatalf("an executive should be able to sign off the contract: %s", expectError(t, exRes))
	}
	if parseJSON(t, exRes)["dod"] != "exec dod" {
		t.Errorf("executive sign-off did not land")
	}
}

// TestUpdateTask_SelfDispatchedFreeFormFieldsStillEditable is AC3: the guard is
// scoped to the contract fields only — the doer can still edit non-contract
// fields (title/description/priority) on its own self-dispatched task.
func TestUpdateTask_SelfDispatchedFreeFormFieldsStillEditable(t *testing.T) {
	h := testHandlers(t)
	taskID := selfDispatch(t, h, "p1", "solo-dev", "")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "solo-dev", "task_id": taskID,
		"title": "renamed by doer", "priority": "P1",
	}))
	if res.IsError {
		t.Fatalf("doer editing non-contract fields on its own task should succeed: %s", expectError(t, res))
	}
	updated := parseJSON(t, res)
	if updated["title"] != "renamed by doer" || updated["priority"] != "P1" {
		t.Errorf("non-contract edit did not land: %+v", updated)
	}
}
