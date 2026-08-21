package relay

import (
	"strings"
	"testing"
)

// TestDispatchTask_MultiBoard_OmittedBoardIDRefused is the repro from 30a455fc:
// dispatch_task without board_id on a project with more than one board used to
// silently land on the oldest board (creation-order) — ~18 tasks mis-filed onto
// the wrong board in production before anyone noticed. It must now be an
// explicit refusal naming every available board, and the task must NOT be
// created.
func TestDispatchTask_MultiBoard_OmittedBoardIDRefused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	_, _ = h.HandleCreateBoard(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "name": "Trovex", "slug": "trovex"}))
	_, _ = h.HandleCreateBoard(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "name": "Wraith", "slug": "wraith"}))

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "unfiled",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "trovex") || !strings.Contains(msg, "wraith") {
		t.Fatalf("refusal must name every available board (slug), got: %s", msg)
	}
	if !strings.Contains(msg, "board_id") {
		t.Errorf("refusal should name the missing arg, got: %s", msg)
	}

	listRes, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	if got := parseJSON(t, listRes)["count"].(float64); got != 0 {
		t.Fatalf("refused dispatch must not create a task, got %v tasks", got)
	}
}

// TestDispatchTask_SingleBoard_OmittedBoardIDStillWorks pins the "unambiguous
// default preserved" half of the fix: exactly one board on the project, and
// board_id omitted, must still land there without any explicit choice.
func TestDispatchTask_SingleBoard_OmittedBoardIDStillWorks(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	boardRes, _ := h.HandleCreateBoard(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "name": "Sprint", "slug": "sprint"}))
	board := parseJSON(t, boardRes)
	boardID := board["id"].(string)

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "lands on the sole board",
	}))
	if res.IsError {
		t.Fatalf("single-board project must keep the implicit default: %s", expectError(t, res))
	}
	task := parseJSON(t, res)["task"].(map[string]any)
	if task["board_id"] != boardID {
		t.Errorf("expected board_id=%s, got %v", boardID, task["board_id"])
	}
}

// TestDispatchTask_MultiBoard_ExplicitBoardIDWorks: naming the board (by full
// UUID or by slug) dispatches normally even with several boards present.
func TestDispatchTask_MultiBoard_ExplicitBoardIDWorks(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	_, _ = h.HandleCreateBoard(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "name": "Trovex", "slug": "trovex"}))
	boardRes, _ := h.HandleCreateBoard(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "name": "Wraith", "slug": "wraith"}))
	board := parseJSON(t, boardRes)
	boardID := board["id"].(string)

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "explicit uuid", "board_id": boardID,
	}))
	if res.IsError {
		t.Fatalf("explicit board_id (uuid) must dispatch: %s", expectError(t, res))
	}

	res2, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "explicit slug", "board_id": "wraith",
	}))
	if res2.IsError {
		t.Fatalf("explicit board_id (slug) must dispatch: %s", expectError(t, res2))
	}
	task2 := parseJSON(t, res2)["task"].(map[string]any)
	if task2["board_id"] != boardID {
		t.Errorf("slug lookup should resolve to %s, got %v", boardID, task2["board_id"])
	}
}

// TestDispatchTask_PriorityWrongType_Refused is the repro from the 30a455fc
// scope-add (msg 9839c920, task 7670216a): priority sent as a non-string
// (e.g. the JSON number 1) used to be silently swallowed by GetString's
// type-mismatch fallback, landing the task on the P2 default. It must now be
// an explicit validation error naming the accepted values.
func TestDispatchTask_PriorityWrongType_Refused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "bad priority", "priority": float64(1),
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "P0") || !strings.Contains(msg, "P1") || !strings.Contains(msg, "P2") || !strings.Contains(msg, "P3") {
		t.Fatalf("refusal must name every accepted priority value, got: %s", msg)
	}

	listRes, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	if got := parseJSON(t, listRes)["count"].(float64); got != 0 {
		t.Fatalf("refused dispatch must not create a task, got %v tasks", got)
	}
}

// TestDispatchTask_PriorityInvalidString_Refused: a syntactically valid string
// that isn't one of P0..P3 is refused too, not just a wrong JSON type.
func TestDispatchTask_PriorityInvalidString_Refused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "bad priority string", "priority": "urgent",
	}))
	expectError(t, res)
}

// TestBatchDispatchTasks_MultiBoard_OmittedBoardIDRefusedPerItem: the shared
// db.DispatchTask choke means batch_dispatch_tasks gets the same board rule for
// free — an item omitting board_id on a multi-board project lands in errors
// (not dispatched), while a sibling item naming its board still succeeds.
func TestBatchDispatchTasks_MultiBoard_OmittedBoardIDRefusedPerItem(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	_, _ = h.HandleCreateBoard(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "name": "Trovex", "slug": "trovex"}))
	boardRes, _ := h.HandleCreateBoard(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "name": "Wraith", "slug": "wraith"}))
	boardID := parseJSON(t, boardRes)["id"].(string)

	res, _ := h.HandleBatchDispatchTasks(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a",
		"tasks": `[{"profile":"dev","title":"named board","board_id":"` + boardID + `"},{"profile":"dev","title":"unfiled"}]`,
	}))
	body := parseJSON(t, res)
	dispatched, _ := body["dispatched"].([]any)
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched, got %d: %+v", len(dispatched), body["dispatched"])
	}
	errs, _ := body["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %+v", len(errs), body["errors"])
	}
	if e := errs[0].(string); !strings.Contains(e, "board_id") {
		t.Errorf("batch error should name the missing board_id, got: %s", e)
	}
}

// TestDispatchTask_PriorityValidString_StillWorks: the ordinary path (a real
// P0..P3 string, or omitted entirely) is untouched by the new validation.
func TestDispatchTask_PriorityValidString_StillWorks(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "P1 task", "priority": "P1",
	}))
	if res.IsError {
		t.Fatalf("valid priority string must dispatch: %s", expectError(t, res))
	}
	task := parseJSON(t, res)["task"].(map[string]any)
	if task["priority"] != "P1" {
		t.Errorf("expected priority=P1, got %v", task["priority"])
	}

	res2, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "default priority",
	}))
	if res2.IsError {
		t.Fatalf("omitted priority must still default: %s", expectError(t, res2))
	}
	task2 := parseJSON(t, res2)["task"].(map[string]any)
	if task2["priority"] != "P2" {
		t.Errorf("expected default priority=P2, got %v", task2["priority"])
	}
}
