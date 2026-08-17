package relay

import (
	"strings"
	"testing"
)

// WRAITH-3: get_message returns the FULL untruncated body that get_inbox /
// get_thread / get_session_context preview-truncate at ~300 chars.
func TestGetMessageReturnsFullContent(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "b", "role": "dev"}))

	long := strings.Repeat("x", 1200)
	sendRes, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "a", "to": "b", "subject": "steer", "content": long,
	}))
	sent := parseJSON(t, sendRes)
	id, _ := sent["id"].(string)
	if id == "" {
		t.Fatalf("no message id in send result: %v", sent)
	}

	// Full ID.
	res, _ := h.HandleGetMessage(ctx, call(map[string]any{"project": "p1", "as": "b", "id": id}))
	got := parseJSON(t, res)
	if c, _ := got["content"].(string); c != long {
		t.Fatalf("expected full %d-char body, got %d chars", len(long), len(c))
	}

	// Unambiguous prefix resolves.
	res2, _ := h.HandleGetMessage(ctx, call(map[string]any{"project": "p1", "as": "b", "id": id[:8]}))
	if c, _ := parseJSON(t, res2)["content"].(string); c != long {
		t.Errorf("prefix lookup did not return full body")
	}

	// Missing id → error.
	if r, _ := h.HandleGetMessage(ctx, call(map[string]any{"project": "p1", "as": "b"})); !r.IsError {
		t.Error("expected error when id omitted")
	}
	// Unknown id → error, not a panic.
	if r, _ := h.HandleGetMessage(ctx, call(map[string]any{"project": "p1", "as": "b", "id": "nope-nope"})); !r.IsError {
		t.Error("expected error for unknown id")
	}
}
