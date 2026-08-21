package relay

import (
	"strings"
	"testing"
)

// TestUpdateTask_DispatcherRescopesContract_Audited is the repro from c229fe44:
// update_task used to silently ignore goal/acceptance_criteria/dod on re-scope.
// The dispatcher re-scoping the ticket must land the new contract (readable
// straight back off the returned task) AND leave the pre-rescope contract
// consultable in the audit trail — never a silent overwrite.
func TestUpdateTask_DispatcherRescopesContract_Audited(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "rescope me",
		"goal": "orig goal", "acceptance_criteria": `["orig ac"]`, "dod": "orig dod",
	}))
	task := parseJSON(t, dispatchRes)["task"].(map[string]any)
	taskID := task["id"].(string)

	updateRes, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID,
		"goal": "new goal", "acceptance_criteria": `["new ac"]`, "dod": "new dod",
	}))
	if updateRes.IsError {
		t.Fatalf("dispatcher re-scope should succeed: %s", expectError(t, updateRes))
	}
	updated := parseJSON(t, updateRes)
	if updated["goal"] != "new goal" {
		t.Errorf("goal not updated: %v", updated["goal"])
	}
	if updated["acceptance_criteria"] != `["new ac"]` {
		t.Errorf("acceptance_criteria not updated: %v", updated["acceptance_criteria"])
	}
	if updated["dod"] != "new dod" {
		t.Errorf("dod not updated: %v", updated["dod"])
	}

	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	got := parseJSON(t, getRes)
	if got["goal"] != "new goal" || got["dod"] != "new dod" {
		t.Fatalf("get_task must show the new contract: %+v", got)
	}

	entries, err := h.db.ListAudit("p1", taskID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "contract_updated" {
			found = true
			if e.Actor != "cto" {
				t.Errorf("audit actor = %q, want cto", e.Actor)
			}
			if !strings.Contains(e.Details, "orig goal") || !strings.Contains(e.Details, "orig dod") {
				t.Errorf("audit details must preserve the old contract, got: %s", e.Details)
			}
			if !strings.Contains(e.Details, "new goal") {
				t.Errorf("audit details must record the new contract too, got: %s", e.Details)
			}
		}
	}
	if !found {
		t.Fatal("expected a contract_updated audit entry, old contract must stay consultable")
	}
}

// TestUpdateTask_AssigneeCannotRescopeContract is the other half of the DoD: an
// assignee attempting to rewrite their own grading bar is refused with a clear
// message, and the contract is left untouched.
func TestUpdateTask_AssigneeCannotRescopeContract(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "protect my contract",
		"goal": "orig goal", "acceptance_criteria": `["orig ac"]`, "dod": "orig dod",
	}))
	task := parseJSON(t, dispatchRes)["task"].(map[string]any)
	taskID := task["id"].(string)

	_, _ = h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "dev-a", "task_id": taskID}))

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "dev-a", "task_id": taskID,
		"goal": "sneaky new goal", "acceptance_criteria": `["sneaky ac"]`, "dod": "sneaky dod",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "dispatcher") {
		t.Errorf("refusal message should name the dispatcher-only rule, got: %s", msg)
	}

	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	got := parseJSON(t, getRes)
	if got["goal"] != "orig goal" || got["acceptance_criteria"] != `["orig ac"]` || got["dod"] != "orig dod" {
		t.Fatalf("contract must be UNCHANGED after a refused assignee attempt, got: %+v", got)
	}
}

// TestUpdateTask_FreeFormFieldsStillWorkForAssignee ensures the dispatcher-only
// guard is scoped to goal/acceptance_criteria/dod only — an assignee must still
// be able to update title/description/priority/progress_note on their own task.
func TestUpdateTask_FreeFormFieldsStillWorkForAssignee(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "free-form ok",
	}))
	task := parseJSON(t, dispatchRes)["task"].(map[string]any)
	taskID := task["id"].(string)

	_, _ = h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "dev-a", "task_id": taskID}))

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "dev-a", "task_id": taskID, "title": "renamed by assignee",
	}))
	if res.IsError {
		t.Fatalf("assignee updating title (not the contract) should succeed: %s", expectError(t, res))
	}
	if parseJSON(t, res)["title"] != "renamed by assignee" {
		t.Errorf("title not updated")
	}
}
