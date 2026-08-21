package relay

import (
	"encoding/json"
	"strings"
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

// A malformed slot (prose instead of an array) is NEVER a 400 and never
// silently dropped — it's folded into the note, so the caller's data survives
// even when the shape is wrong (accept-and-slot, d601ac41).
func TestSendStatusMalformedSlotFoldedIntoNote(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "worker", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "lead", "role": "lead"}))

	res, _ := h.HandleSendStatus(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "to": "lead",
		"done": "finished the auth work", // prose, not an array — malformed shape
	}))
	if res.IsError {
		t.Fatalf("malformed slot must never 400: %v", res)
	}
	sent := parseJSON(t, res)
	id, _ := sent["id"].(string)

	got := parseJSON(t, mustGetMessage(t, h, "p1", "lead", id))
	meta, _ := got["metadata"].(string)
	var p statusPayload
	if err := json.Unmarshal([]byte(meta), &p); err != nil {
		t.Fatalf("metadata not a status schema: %v (%q)", err, meta)
	}
	if len(p.Done) != 0 {
		t.Errorf("malformed done must not parse as a slot item, got %v", p.Done)
	}
	if p.Note == "" || !strings.Contains(p.Note, "finished the auth work") {
		t.Errorf("malformed done text must survive in the note, got %q", p.Note)
	}
}

// A stray non-string item inside an otherwise well-formed array is folded into
// the note too, not silently skipped — the well-formed items still slot
// normally.
func TestSendStatusStrayItemFoldedIntoNote(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "worker", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "lead", "role": "lead"}))

	res, _ := h.HandleSendStatus(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "to": "lead",
		"blockers": []any{"real blocker", float64(3)},
	}))
	if res.IsError {
		t.Fatalf("stray item must never 400: %v", res)
	}
	sent := parseJSON(t, res)
	id, _ := sent["id"].(string)

	got := parseJSON(t, mustGetMessage(t, h, "p1", "lead", id))
	meta, _ := got["metadata"].(string)
	var p statusPayload
	if err := json.Unmarshal([]byte(meta), &p); err != nil {
		t.Fatalf("metadata not a status schema: %v (%q)", err, meta)
	}
	if len(p.Blockers) != 1 || p.Blockers[0] != "real blocker" {
		t.Errorf("well-formed item must still slot normally, got %v", p.Blockers)
	}
	if p.Note == "" || !strings.Contains(p.Note, "3") {
		t.Errorf("stray non-string item must survive in the note, got %q", p.Note)
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
