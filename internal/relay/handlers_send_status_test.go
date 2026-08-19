package relay

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// send_status posts a no-wake typed report: type 'status', action_required
// 'none', metadata carrying the slotted schema — and a listed blocker at P2
// SURFACES without waking the recipient (Ruling-3).
func TestSendStatusNoWakeWithBlockers(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "worker", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "lead", "role": "lead"}))

	res, _ := h.HandleSendStatus(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "to": "lead",
		"done":     []any{"merged S4"},
		"doing":    []any{"S3 typed status"},
		"blockers": []any{"waiting on redeploy"},
		"note":     "on track",
	}))
	if res.IsError {
		t.Fatalf("send_status errored: %v", res)
	}
	sent := parseJSON(t, res)
	id, _ := sent["id"].(string)
	if id == "" {
		t.Fatalf("no message id: %v", sent)
	}

	// Fetch full message; assert type + action_required + metadata schema.
	got := parseJSON(t, mustGetMessage(t, h, "p1", "lead", id))
	if got["type"] != "status" {
		t.Errorf("type = %v, want status", got["type"])
	}
	meta, _ := got["metadata"].(string)
	var p statusPayload
	if err := json.Unmarshal([]byte(meta), &p); err != nil {
		t.Fatalf("metadata not a status schema: %v (%q)", err, meta)
	}
	if len(p.Blockers) != 1 || p.Blockers[0] != "waiting on redeploy" {
		t.Errorf("blockers slot = %v", p.Blockers)
	}

	// The core invariant: a P2 status with a blocker does NOT wake the lead.
	n, err := h.db.UnreadCountForAgent("p1", "lead")
	if err != nil {
		t.Fatalf("unread count: %v", err)
	}
	if n != 0 {
		t.Errorf("status woke the lead (unread=%d), want 0 — blockers are surfaced, not woken", n)
	}
}

// A P0 status is the deliberate escape hatch: the guard-first predicate's P0
// clause still wakes even though action_required is 'none'.
func TestSendStatusP0StillWakes(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "worker", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "lead", "role": "lead"}))

	if r, _ := h.HandleSendStatus(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "to": "lead", "priority": "P0",
		"blockers": []any{"prod relay down"},
	})); r.IsError {
		t.Fatalf("send_status P0 errored: %v", r)
	}
	n, _ := h.db.UnreadCountForAgent("p1", "lead")
	if n != 1 {
		t.Errorf("P0 status unread=%d, want 1 (P0 always wakes)", n)
	}
}

// to is required.
func TestSendStatusRequiresRecipient(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "worker", "role": "dev"}))
	if r, _ := h.HandleSendStatus(ctx, call(map[string]any{"project": "p1", "as": "worker", "done": []any{"x"}})); !r.IsError {
		t.Error("expected error when 'to' omitted")
	}
}

func mustGetMessage(t *testing.T, h *Handlers, project, as, id string) *mcp.CallToolResult {
	t.Helper()
	res, _ := h.HandleGetMessage(ctx, call(map[string]any{"project": project, "as": as, "id": id}))
	if res.IsError {
		t.Fatalf("get_message errored for %s", id)
	}
	return res
}
