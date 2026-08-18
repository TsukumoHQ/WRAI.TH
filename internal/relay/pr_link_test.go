package relay

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func dispatchTaskID(t *testing.T, h *Handlers) string {
	t.Helper()
	registerActive(t, h, "p1", "bot-a", nil)
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "pr me",
	}))
	if res.IsError {
		t.Fatalf("dispatch: %s", expectError(t, res))
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &body)
	task, _ := body["task"].(map[string]any)
	id, _ := task["id"].(string)
	if id == "" {
		t.Fatalf("no task id: %v", body)
	}
	return id
}

func linkResult(t *testing.T, h *Handlers, args map[string]any) map[string]any {
	t.Helper()
	res, _ := h.HandleLinkPr(ctx, call(args))
	if res.IsError {
		t.Fatalf("link_pr error: %s", expectError(t, res))
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &body)
	return body
}

func TestLinkPrStoresAndSurfaces(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)

	// link_pr returns the task at top level (like claim/review).
	task := linkResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id,
		"pr_number": 164, "pr_url": "https://github.com/o/r/pull/164",
		"pr_repo": "o/r", "pr_state": "open",
	})
	if task["pr_number"] != float64(164) || task["pr_state"] != "open" || task["pr_repo"] != "o/r" {
		t.Fatalf("pr zone not stored on task: %v", task)
	}

	// get_task must carry the PR zone (scan lockstep).
	gres, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": id}))
	var gtask map[string]any
	_ = json.Unmarshal([]byte(gres.Content[0].(mcp.TextContent).Text), &gtask)
	if gtask["pr_url"] != "https://github.com/o/r/pull/164" {
		t.Fatalf("get_task lost pr_url: %v", gtask)
	}
}

// A state-only re-link must NOT wipe url/number/repo (COALESCE).
func TestLinkPrPartialUpdateKeepsFields(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)
	linkResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id,
		"pr_number": 7, "pr_url": "https://github.com/o/r/pull/7", "pr_repo": "o/r", "pr_state": "open",
	})
	// Only advance the state.
	task := linkResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "merged",
	})
	if task["pr_state"] != "merged" {
		t.Fatalf("state not advanced: %v", task)
	}
	if task["pr_number"] != float64(7) || task["pr_url"] != "https://github.com/o/r/pull/7" {
		t.Fatalf("partial update wiped url/number: %v", task)
	}
}

func TestLinkPrInvalidStateRejected(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)
	res, _ := h.HandleLinkPr(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_state": "bogus",
	}))
	if !res.IsError {
		t.Fatal("expected validation error for bad pr_state")
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &body)
	if body["errorCategory"] != CategoryValidation {
		t.Fatalf("bad pr_state should be a validation error: %v", body)
	}
}

func TestLinkPrMissingTaskNotFound(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "bot-a", nil)
	res, _ := h.HandleLinkPr(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "task_id": "00000000-0000-0000-0000-000000000000", "pr_number": 1,
	}))
	if !res.IsError {
		t.Fatal("expected NOT_FOUND for missing task")
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &body)
	if body["code"] != CodeNotFound {
		t.Fatalf("expected NOT_FOUND code: %v", body)
	}
}

// PR zone survives a task state transition (scan↔SELECT lockstep intact).
func TestPrZoneSurvivesTransition(t *testing.T) {
	h := testHandlers(t)
	id := dispatchTaskID(t, h)
	linkResult(t, h, map[string]any{
		"project": "p1", "as": "bot-a", "task_id": id, "pr_number": 42, "pr_state": "open",
	})
	if r, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": id})); r.IsError {
		t.Fatalf("claim: %s", expectError(t, r))
	}
	task, err := h.db.GetTask(id, "p1")
	if err != nil || task == nil {
		t.Fatalf("get task: %v", err)
	}
	if task.PRNumber == nil || *task.PRNumber != 42 {
		t.Fatalf("pr_number lost through transition: %+v", task.PRNumber)
	}
}
