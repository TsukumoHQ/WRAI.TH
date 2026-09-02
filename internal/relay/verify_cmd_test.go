package relay

import (
	"strings"
	"testing"

	"agent-relay/internal/models"
)

// verify_cmd (task 6c1c5167 follow-up, DEC-niwa-goal-validate-1) is the
// OPTIONAL sibling of goal/acceptance_criteria/dod: a complete typed ticket
// missing only verify_cmd must still dispatch on niwa (seeded enforced) —
// its absence never triggers the typed-ticket refusal.
func TestDispatchTask_VerifyCmd_OptionalEvenWhenEnforced(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "niwa", "as": "cto", "profile": "dev", "title": "no verify_cmd",
		"goal": "g", "acceptance_criteria": `["a"]`, "dod": "d",
	}))
	if res.IsError {
		t.Fatalf("a complete ticket with no verify_cmd must dispatch on an enforced project: %s", expectError(t, res))
	}
	task := parseJSON(t, res)["task"].(map[string]any)
	if v, ok := task["verify_cmd"]; ok && v != nil {
		t.Errorf("expected absent/nil verify_cmd, got %v", v)
	}
}

// A supplied verify_cmd persists and reads back through get_task, verbatim —
// no relay-side validation or mutation of the command string.
func TestDispatchTask_VerifyCmd_PersistsAndReadsBack(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "niwa", "as": "cto", "profile": "dev", "title": "with verify_cmd",
		"goal": "g", "acceptance_criteria": `["a"]`, "dod": "d",
		"verify_cmd": `go test ./... -run TestFoo`,
	}))
	if res.IsError {
		t.Fatalf("dispatch: %s", expectError(t, res))
	}
	task := parseJSON(t, res)["task"].(map[string]any)
	if task["verify_cmd"] != `go test ./... -run TestFoo` {
		t.Fatalf("verify_cmd not persisted on dispatch response: %v", task["verify_cmd"])
	}
	taskID := task["id"].(string)

	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "niwa", "task_id": taskID}))
	got := parseJSON(t, getRes)
	if got["verify_cmd"] != `go test ./... -run TestFoo` {
		t.Errorf("get_task lost verify_cmd: %v", got["verify_cmd"])
	}

	listRes, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "niwa", "format": "json"}))
	tasks := parseJSON(t, listRes)["tasks"].([]any)
	found := false
	for _, tt := range tasks {
		tm := tt.(map[string]any)
		if tm["id"] == taskID {
			found = true
			if tm["verify_cmd"] != `go test ./... -run TestFoo` {
				t.Errorf("list_tasks lost verify_cmd: %v", tm["verify_cmd"])
			}
		}
	}
	if !found {
		t.Fatal("dispatched task not found in list_tasks")
	}
}

// batch_dispatch_tasks accepts verify_cmd per item and persists it, same as
// the single-dispatch path.
func TestBatchDispatch_VerifyCmd_PerItem(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleBatchDispatchTasks(ctx, call(map[string]any{
		"project": "niwa", "as": "cto",
		"tasks": `[
			{"profile":"dev","title":"batch with cmd","goal":"g","acceptance_criteria":["a"],"dod":"d","verify_cmd":"make verify"},
			{"profile":"dev","title":"batch no cmd","goal":"g","acceptance_criteria":["a"],"dod":"d"}
		]`,
	}))
	body := parseJSON(t, res)
	dispatched, _ := body["dispatched"].([]any)
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched, got %d: %+v", len(dispatched), body)
	}

	listRes, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "niwa", "format": "json"}))
	tasks := parseJSON(t, listRes)["tasks"].([]any)
	var withCmd, noCmd bool
	for _, tt := range tasks {
		tm := tt.(map[string]any)
		switch tm["title"] {
		case "batch with cmd":
			if tm["verify_cmd"] != "make verify" {
				t.Errorf("batch item verify_cmd not persisted: %v", tm["verify_cmd"])
			}
			withCmd = true
		case "batch no cmd":
			if v, ok := tm["verify_cmd"]; ok && v != nil {
				t.Errorf("batch item without verify_cmd should read nil, got %v", v)
			}
			noCmd = true
		}
	}
	if !withCmd || !noCmd {
		t.Fatalf("expected both batch items present, got tasks: %+v", tasks)
	}
}

// update_task's verify_cmd rides the same dispatcher-only + audited gate as
// goal/acceptance_criteria/dod (task 6c1c5167 follow-up spec: "audited
// exactly like goal/acceptance_criteria/dod").
func TestUpdateTask_VerifyCmd_DispatcherOnly_Audited(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))

	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "verify_cmd rescope",
	}))
	task := parseJSON(t, dispatchRes)["task"].(map[string]any)
	taskID := task["id"].(string)

	// Dispatcher can set it.
	updateRes, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID, "verify_cmd": "go build ./...",
	}))
	if updateRes.IsError {
		t.Fatalf("dispatcher setting verify_cmd should succeed: %s", expectError(t, updateRes))
	}
	if parseJSON(t, updateRes)["verify_cmd"] != "go build ./..." {
		t.Errorf("verify_cmd not updated: %v", parseJSON(t, updateRes)["verify_cmd"])
	}

	entries, err := h.db.ListAudit("p1", taskID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "contract_updated" && strings.Contains(e.Details, "go build ./...") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a contract_updated audit entry recording the new verify_cmd")
	}

	// The assignee cannot re-scope it, same as goal/acceptance_criteria/dod.
	_, _ = h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "dev-a", "task_id": taskID}))
	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "dev-a", "task_id": taskID, "verify_cmd": "sneaky command",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "dispatcher") {
		t.Errorf("refusal should name the dispatcher-only rule, got: %s", msg)
	}

	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	if got := parseJSON(t, getRes)["verify_cmd"]; got != "go build ./..." {
		t.Fatalf("verify_cmd must be UNCHANGED after a refused assignee attempt, got: %v", got)
	}
}

// get_session_context's task previews (TaskSummary) must also carry
// verify_cmd — the task's own acceptance criteria names get_task / list_tasks
// / get_session_context explicitly, all three.
func TestSummarizeTask_CarriesVerifyCmd(t *testing.T) {
	cmd := "go vet ./..."
	s := summarizeTask(models.Task{
		ID: "abc", Title: "t", Priority: "P2", Status: "pending",
		VerifyCmd: &cmd,
	})
	if s.VerifyCmd == nil || *s.VerifyCmd != cmd {
		t.Fatalf("summarizeTask dropped verify_cmd, got %v, want %q", s.VerifyCmd, cmd)
	}

	absent := summarizeTask(models.Task{ID: "abc", Title: "t", Priority: "P2", Status: "pending"})
	if absent.VerifyCmd != nil {
		t.Fatalf("expected nil verify_cmd when absent, got %q", *absent.VerifyCmd)
	}
}
