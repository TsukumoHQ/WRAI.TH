package relay

import (
	"strings"
	"testing"
)

// TestMemoryDiscipline_OptInOnly verifies the write-side guard
// (DEC-governance-memory-protocol-2) only fires on a project that opted in
// (DEC-governance-enforcement-1: never imposed by default) — a checkpoint-like
// key, empty tags, and area="general" all pass through untouched on a plain
// project.
func TestMemoryDiscipline_OptInOnly(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "plain", "as": "bot-a", "key": "agent-checkpoint-1", "value": "some state",
	}))
	if res.IsError {
		t.Fatalf("expected checkpoint-key write to pass on a non-enforced project, got error: %s", res.Content[0])
	}

	res2, _ := h.HandleRemember(ctx, call(map[string]any{
		"project": "plain", "as": "bot-a", "decision": "use X", "area": "general",
	}))
	if res2.IsError {
		t.Fatalf("expected area=general to pass on a non-enforced project, got error: %s", res2.Content[0])
	}
}

// TestMemoryDiscipline_SetMemoryRejections exercises every set_memory
// rejection path once the project has opted in.
func TestMemoryDiscipline_SetMemoryRejections(t *testing.T) {
	h := testHandlers(t)
	if err := h.db.SetProjectRequiresMemoryDiscipline("tsukumo", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}

	// Checkpoint-looking key.
	res, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "tsukumo", "as": "bot-a", "key": "agent-checkpoint-1", "value": "some state", "tags": []any{"wraith"},
	}))
	if msg := expectError(t, res); !strings.Contains(msg, "checkpoint") {
		t.Errorf("expected a checkpoint hint, got %q", msg)
	}

	// Empty tags.
	res2, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "tsukumo", "as": "bot-a", "key": "wraith-serve-wedge", "value": "root cause: writer pool wedge",
	}))
	if msg := expectError(t, res2); !strings.Contains(msg, "tag") {
		t.Errorf("expected a tags hint, got %q", msg)
	}

	// layer=context missing valid_until.
	res3, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "tsukumo", "as": "bot-a", "key": "wraith-serve-wedge", "value": "root cause: writer pool wedge",
		"tags": []any{"wraith"}, "layer": "context",
	}))
	if msg := expectError(t, res3); !strings.Contains(msg, "valid_until") {
		t.Errorf("expected a valid_until hint, got %q", msg)
	}

	// layer=context, valid_until too far out.
	res4, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "tsukumo", "as": "bot-a", "key": "wraith-serve-wedge", "value": "root cause: writer pool wedge",
		"tags": []any{"wraith"}, "layer": "context", "valid_until": "2030-01-01T00:00:00Z",
	}))
	if msg := expectError(t, res4); !strings.Contains(msg, "valid_until") {
		t.Errorf("expected a valid_until hint for a too-far window, got %q", msg)
	}

	// A clean write passes.
	res5, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "tsukumo", "as": "bot-a", "key": "wraith-serve-wedge", "value": "root cause: writer pool wedge",
		"tags": []any{"wraith"},
	}))
	if res5.IsError {
		t.Fatalf("expected a clean write to pass, got error: %s", res5.Content[0])
	}
}

// TestMemoryDiscipline_ValueWarning verifies the over-600-char value is
// accepted but flagged, never blocked, once the project has opted in.
func TestMemoryDiscipline_ValueWarning(t *testing.T) {
	h := testHandlers(t)
	if err := h.db.SetProjectRequiresMemoryDiscipline("tsukumo", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}

	long := strings.Repeat("a", 601)
	res, _ := h.HandleSetMemory(ctx, call(map[string]any{
		"project": "tsukumo", "as": "bot-a", "key": "wraith-long-value", "value": long, "tags": []any{"wraith"},
	}))
	data := parseJSON(t, res)
	if data["warning"] == nil {
		t.Error("expected a warning annotation for a 601-char value, got none")
	}
}

// TestMemoryDiscipline_RememberRejectsGeneral verifies remember() rejects
// area="general" once the project has opted in.
func TestMemoryDiscipline_RememberRejectsGeneral(t *testing.T) {
	h := testHandlers(t)
	if err := h.db.SetProjectRequiresMemoryDiscipline("tsukumo", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}

	res, _ := h.HandleRemember(ctx, call(map[string]any{
		"project": "tsukumo", "as": "bot-a", "decision": "use X", "area": "general",
	}))
	if msg := expectError(t, res); !strings.Contains(msg, "general") {
		t.Errorf("expected an area=general hint, got %q", msg)
	}

	res2, _ := h.HandleRemember(ctx, call(map[string]any{
		"project": "tsukumo", "as": "bot-a", "decision": "use X", "area": "wraith/memory",
	}))
	if res2.IsError {
		t.Fatalf("expected a real area to pass, got error: %s", res2.Content[0])
	}
}
