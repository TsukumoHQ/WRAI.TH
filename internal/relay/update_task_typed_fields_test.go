package relay

import (
	"strings"
	"testing"
)

// dispatchForTypedFieldTest dispatches a task as "cto" (so cto is the dispatcher
// and may edit the contract) and returns its id.
func dispatchForTypedFieldTest(t *testing.T, h *Handlers, title string) string {
	t.Helper()
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev-a", "role": "dev"}))
	dispatchRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": title,
		"goal": "orig goal", "acceptance_criteria": `["orig ac"]`, "dod": "orig dod",
	}))
	if dispatchRes.IsError {
		t.Fatalf("dispatch: %s", expectError(t, dispatchRes))
	}
	return parseJSON(t, dispatchRes)["task"].(map[string]any)["id"].(string)
}

// AC1: acceptance_criteria passed as a NATIVE array of strings is coerced to its
// canonical JSON-string form and applied — and a progress_note in the SAME call
// is applied too (the 3ad80aae repro shape that used to 200 + silently drop both).
func TestUpdateTask_NativeArrayACCoercedWithProgressNote(t *testing.T) {
	h := testHandlers(t)
	taskID := dispatchForTypedFieldTest(t, h, "native array ac")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID,
		"acceptance_criteria": []any{"AC one thing", "AC two thing"},
		"progress_note":       "made progress",
	}))
	if res.IsError {
		t.Fatalf("native-array AC + progress_note should apply, got: %s", expectError(t, res))
	}
	const wantCanonical = `["AC one thing","AC two thing"]`
	if got := parseJSON(t, res)["acceptance_criteria"]; got != wantCanonical {
		t.Errorf("acceptance_criteria = %v, want canonical JSON-string %q", got, wantCanonical)
	}

	// The stored value (read back) is the canonical JSON string, not dropped.
	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	if got := parseJSON(t, getRes)["acceptance_criteria"]; got != wantCanonical {
		t.Errorf("stored acceptance_criteria = %v, want %q", got, wantCanonical)
	}

	// The progress_note in the same call was applied, not dropped.
	notes, err := h.db.GetProgressNotes(taskID, "p1")
	if err != nil {
		t.Fatalf("GetProgressNotes: %v", err)
	}
	found := false
	for _, n := range notes {
		if n.Note == "made progress" {
			found = true
		}
	}
	if !found {
		t.Errorf("progress_note dropped: notes=%+v", notes)
	}
}

// AC2: a wrong-typed value on a string field refuses INVALID_ARGUMENT naming the
// field and the expected type, and NO field from the request is applied — atomic
// refusal, last_activity_at unchanged (the valid sibling field does not land).
func TestUpdateTask_WrongTypedStringFieldRefusedAtomically(t *testing.T) {
	h := testHandlers(t)
	taskID := dispatchForTypedFieldTest(t, h, "atomic refusal")

	before, _ := h.db.GetTask(taskID, "p1")
	beforeLA := ""
	if before.LastActivityAt != nil {
		beforeLA = *before.LastActivityAt
	}

	// title is the wrong type (a number); description is a valid sibling edit that
	// must NOT be applied because the whole request is refused atomically.
	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID,
		"title":       42,
		"description": "should not be applied",
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "title") {
		t.Errorf("refusal must name the field 'title', got: %s", msg)
	}
	if !strings.Contains(msg, "string") {
		t.Errorf("refusal must name the expected type 'string', got: %s", msg)
	}

	after, _ := h.db.GetTask(taskID, "p1")
	if after.Description == "should not be applied" {
		t.Error("the valid sibling field was applied despite the atomic refusal")
	}
	if after.Title != before.Title {
		t.Errorf("title changed on a refused request: %q -> %q", before.Title, after.Title)
	}
	afterLA := ""
	if after.LastActivityAt != nil {
		afterLA = *after.LastActivityAt
	}
	if afterLA != beforeLA {
		t.Errorf("last_activity_at changed on a refused request: %q -> %q", beforeLA, afterLA)
	}
}

// AC3: acceptance_criteria as an array containing a non-string element is refused
// INVALID_ARGUMENT naming the field — neither coerced nor dropped.
func TestUpdateTask_ACArrayWithNonStringRefused(t *testing.T) {
	h := testHandlers(t)
	taskID := dispatchForTypedFieldTest(t, h, "ac non-string element")

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "task_id": taskID,
		"acceptance_criteria": []any{"ok item", 5},
	}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "acceptance_criteria") {
		t.Errorf("refusal must name 'acceptance_criteria', got: %s", msg)
	}

	// Untouched: the original contract stands (not coerced to a partial, not dropped).
	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	if got := parseJSON(t, getRes)["acceptance_criteria"]; got != `["orig ac"]` {
		t.Errorf("acceptance_criteria must be unchanged after refusal, got: %v", got)
	}
}
