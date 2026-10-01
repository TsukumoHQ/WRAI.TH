package relay

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// W10 dd6b1ab3 — a send to an inactive non-service agent still delivers (the
// inbox is non-destructive) but tells the sender, at send time, that nobody is
// reading: warning recipient_inactive {name, status, last_seen}.

func inactiveFleet(t *testing.T) *Handlers {
	h := testHandlers(t)
	for _, a := range []map[string]any{
		{"project": "p1", "name": "bob"},
		{"project": "p1", "name": "alice"},
		{"project": "p1", "name": "carol"},
		{"project": "p1", "name": "svc", "is_service": true},
	} {
		if res, _ := h.HandleRegisterAgent(ctx, call(a)); res.IsError {
			t.Fatalf("register %v: %s", a["name"], expectError(t, res))
		}
	}
	for _, name := range []string{"carol", "svc"} {
		if err := h.db.DeactivateAgent("p1", name); err != nil {
			t.Fatalf("deactivate %s: %v", name, err)
		}
	}
	return h
}

// sendOut sends as bob and returns the decoded success body.
func sendOut(t *testing.T, h *Handlers, to, msgType string) map[string]any {
	t.Helper()
	args := map[string]any{"project": "p1", "as": "bob", "to": to, "content": "ping " + to}
	if msgType != "" {
		args["type"] = msgType
	}
	res, _ := h.HandleSendMessage(ctx, call(args))
	if res.IsError {
		t.Fatalf("send to %s (%s) must never be refused: %s", to, msgType, expectError(t, res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out)
	return out
}

// AC1 — delivered, and the response carries recipient_inactive with name and
// last_seen.
func TestSendToInactiveRecipientWarns(t *testing.T) {
	h := inactiveFleet(t)
	out := sendOut(t, h, "carol", "")
	w, _ := out["warning"].(map[string]any)
	if w == nil || w["code"] != "recipient_inactive" || w["name"] != "carol" || w["status"] != "inactive" || w["last_seen"] == "" || w["last_seen"] == nil {
		t.Fatalf("want warning recipient_inactive {name carol, status, last_seen}, got %v", out["warning"])
	}
	if out["id"] == nil || out["id"] == "" {
		t.Fatalf("the message itself must be stored, got %v", out)
	}
	msgs, err := h.db.GetInbox("p1", "carol", true, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the message must still be delivered to carol's inbox, got %d (%v)", len(msgs), err)
	}
}

// AC2 — a send to an active agent carries no warning (shape unchanged).
func TestSendToActiveRecipientNoWarning(t *testing.T) {
	h := inactiveFleet(t)
	if out := sendOut(t, h, "alice", ""); out["warning"] != nil {
		t.Fatalf("active recipient: no warning expected, got %v", out["warning"])
	}
}

// AC3 — ack/fyi-type sends, and sends to an is_service recipient, carry no
// warning.
func TestSendInactiveWarningExemptions(t *testing.T) {
	h := inactiveFleet(t)
	for _, typ := range []string{"ack", "fyi"} {
		if out := sendOut(t, h, "carol", typ); out["warning"] != nil {
			t.Fatalf("%s-type send: no warning expected, got %v", typ, out["warning"])
		}
	}
	if out := sendOut(t, h, "svc", ""); out["warning"] != nil {
		t.Fatalf("service recipient: no warning expected, got %v", out["warning"])
	}
}
