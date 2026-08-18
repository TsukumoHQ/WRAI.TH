package relay

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// guardedHandler returns the registry-wrapped handler for a tool by name, so
// tests exercise the real guardIdentity + T2 liveness gate the server installs
// (not the bare handler).
func guardedHandler(t *testing.T, h *Handlers, name string) server.ToolHandlerFunc {
	t.Helper()
	for _, rt := range h.toolRegistry() {
		if rt.Tool.Name == name {
			return rt.Handler
		}
	}
	t.Fatalf("tool %q not found in registry", name)
	return nil
}

// expectSenderInactive asserts an error result carries the typed SENDER_INACTIVE
// code (JSON body, not a bare string) and returns the reason — so a client can
// branch on the code and PARK instead of hot-looping.
func expectSenderInactive(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatal("expected a typed error result, got success/nil")
	}
	raw := res.Content[0].(mcp.TextContent).Text
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("refusal is not structured JSON (client can't branch on it): %v\nraw: %s", err, raw)
	}
	if body["code"] != "SENDER_INACTIVE" {
		t.Fatalf("want code SENDER_INACTIVE, got %v\nraw: %s", body["code"], raw)
	}
	reason, _ := body["reason"].(string)
	return reason
}

func registerActive(t *testing.T, h *Handlers, project, name string, extra map[string]any) {
	t.Helper()
	args := map[string]any{"project": project, "name": name, "role": "dev"}
	for k, v := range extra {
		args[k] = v
	}
	if r, _ := h.HandleRegisterAgent(ctx, call(args)); r.IsError {
		t.Fatalf("register %s: %s", name, expectError(t, r))
	}
}

// TestSenderGate_InactiveGetsTypedRefusal: a genuinely inactive non-service
// sender's send is refused with the typed SENDER_INACTIVE code (the anti-
// hot-loop signal).
func TestSenderGate_InactiveGetsTypedRefusal(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "worker", nil)
	if err := h.db.DeactivateAgent("p1", "worker"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	send := guardedHandler(t, h, "send_message")
	res, _ := send(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "to": "user", "content": "feedback",
	}))
	if reason := expectSenderInactive(t, res); reason != "inactive" {
		t.Fatalf("want reason inactive, got %q", reason)
	}
	// A second identical send must ALSO be typed — the client never gets a
	// transient-looking error it might retry into a loop.
	res2, _ := send(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "to": "user", "content": "feedback",
	}))
	_ = expectSenderInactive(t, res2)
}

// TestSenderGate_UnregisteredGetsTypedRefusal: an unregistered sender on a real
// project gets SENDER_INACTIVE (reason unregistered) on the send path — same
// park signal, not the generic "not registered" string.
func TestSenderGate_UnregisteredGetsTypedRefusal(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "anchor", nil) // makes p1 a real project
	send := guardedHandler(t, h, "send_message")
	res, _ := send(ctx, call(map[string]any{
		"project": "p1", "as": "ghost", "to": "user", "content": "hi",
	}))
	if reason := expectSenderInactive(t, res); reason != "unregistered" {
		t.Fatalf("want reason unregistered, got %q", reason)
	}
}

// TestSenderGate_ServiceExempt: a service identity sends successfully even after
// it is marked inactive — the dead-fleet QA daemon still posts feedback.
func TestSenderGate_ServiceExempt(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "qa-daemon", map[string]any{"is_service": true})
	if err := h.db.DeactivateAgent("p1", "qa-daemon"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	send := guardedHandler(t, h, "send_message")
	res, _ := send(ctx, call(map[string]any{
		"project": "p1", "as": "qa-daemon", "to": "user", "content": "worker X is down",
	}))
	if res != nil && res.IsError {
		t.Fatalf("service daemon send should succeed even when inactive: %s", res.Content[0].(mcp.TextContent).Text)
	}
}

// TestSenderGate_AckDeliverySameModel: ack_delivery uses the same gate, so an
// inactive sender is parked before any delivery lookup (consistency with T4).
func TestSenderGate_AckDeliverySameModel(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "worker", nil)
	if err := h.db.DeactivateAgent("p1", "worker"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	ack := guardedHandler(t, h, "ack_delivery")
	res, _ := ack(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "delivery_id": "does-not-matter",
	}))
	_ = expectSenderInactive(t, res)
}

// TestIsEligible_MatchesVerdictAndDoesNotWrite: the read-only check returns the
// same verdict a send would, and never mutates the agent (no reactivation).
func TestIsEligible_MatchesVerdictAndDoesNotWrite(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "carol", nil)

	// active → eligible
	res, _ := h.HandleIsEligible(ctx, call(map[string]any{"project": "p1", "agent": "carol"}))
	got := parseJSON(t, res)
	if got["eligible"] != true || got["reason"] != "active" {
		t.Fatalf("active agent: want eligible=true reason=active, got %v", got)
	}

	if err := h.db.DeactivateAgent("p1", "carol"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// inactive → ineligible, and the check itself writes nothing
	res, _ = h.HandleIsEligible(ctx, call(map[string]any{"project": "p1", "agent": "carol"}))
	got = parseJSON(t, res)
	if got["eligible"] != false || got["reason"] != "inactive" {
		t.Fatalf("inactive agent: want eligible=false reason=inactive, got %v", got)
	}
	agent, _ := h.db.GetAgent("p1", "carol")
	if agent == nil || agent.Status != "inactive" {
		t.Fatalf("is_eligible must not mutate the agent (still inactive), got %+v", agent)
	}
}
