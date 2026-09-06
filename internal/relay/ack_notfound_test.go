package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// expectNotFound asserts an error result carries the typed NOT_FOUND envelope
// (structured JSON, not a bare string) so a caller can branch on the code and
// know its ack did NOT land — the opposite of the silent-success bug.
func expectNotFound(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatal("expected a typed NOT_FOUND error result, got success/nil (silent-success regression)")
	}
	raw := res.Content[0].(mcp.TextContent).Text
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("refusal is not structured JSON (client can't branch on it): %v\nraw: %s", err, raw)
	}
	if body["code"] != "NOT_FOUND" {
		t.Fatalf("want code NOT_FOUND, got %v\nraw: %s", body["code"], raw)
	}
	return body
}

// TestAckDelivery_BogusMessageIDRefusesNotFound (bf920f6c-class reproduction):
// ack_delivery via message_id with an id that resolves NOTHING used to echo
// {acknowledged_message_id: <bogus>} — a silent success while the real unread
// stayed unread. It must now refuse loudly with a typed NOT_FOUND naming the id.
func TestAckDelivery_BogusMessageIDRefusesNotFound(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "worker", nil)
	ack := guardedHandler(t, h, "ack_delivery")

	bogus := "ffbf98ef-2c3a-4bfe-95cf-c3e43d72ac7f" // the live-repro id
	res, _ := ack(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "message_id": bogus,
	}))
	body := expectNotFound(t, res)
	if msg, _ := body["message"].(string); !strings.Contains(msg, bogus) {
		t.Fatalf("NOT_FOUND message must name the id %q, got %q", bogus, msg)
	}
}

// TestAckDelivery_ValidMessageIDStillAcks: the valid path is byte-for-byte
// behaviour — a real unread message_id acks and echoes acknowledged_message_id —
// and a second ack of the SAME real id (delivery already acked) stays a success
// (idempotent), never a false NOT_FOUND.
func TestAckDelivery_ValidMessageIDStillAcks(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "sender", nil)
	registerActive(t, h, "p1", "worker", nil)
	m, _, err := h.db.InsertMessageWithDeliveries("p1", "sender", "worker", "notification", "hi", "body", "{}", "P2", 0, nil, nil, []string{"worker"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	ack := guardedHandler(t, h, "ack_delivery")

	res, _ := ack(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "message_id": m.ID,
	}))
	if res != nil && res.IsError {
		t.Fatalf("valid ack should succeed: %s", res.Content[0].(mcp.TextContent).Text)
	}
	got := parseJSON(t, res)
	if got["acknowledged_message_id"] != m.ID {
		t.Fatalf("want acknowledged_message_id=%s, got %v", m.ID, got["acknowledged_message_id"])
	}

	// Idempotent re-ack of a REAL id must not be mistaken for a bogus one.
	res2, _ := ack(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "message_id": m.ID,
	}))
	if res2 != nil && res2.IsError {
		t.Fatalf("idempotent re-ack of a real id must not refuse: %s", res2.Content[0].(mcp.TextContent).Text)
	}
}

// TestAckDelivery_CrossProjectIDRefusesNotFound: a message_id that exists only
// in ANOTHER project resolves nothing in the caller's project, so it refuses
// NOT_FOUND — no cross-project silent ack.
func TestAckDelivery_CrossProjectIDRefusesNotFound(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p2", "sender", nil)
	registerActive(t, h, "p2", "worker", nil)
	registerActive(t, h, "p1", "worker", nil)
	m, _, err := h.db.InsertMessageWithDeliveries("p2", "sender", "worker", "notification", "hi", "body", "{}", "P2", 0, nil, nil, []string{"worker"}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	ack := guardedHandler(t, h, "ack_delivery")

	// worker acting in p1 tries to ack a p2-scoped message id.
	res, _ := ack(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "message_id": m.ID,
	}))
	_ = expectNotFound(t, res)

	// The real p2 delivery stayed unread — the failed cross-project ack didn't drain it.
	if err := h.db.AcknowledgeDeliveryByMessage(m.ID, "worker", "p2"); err != nil {
		t.Fatalf("the genuine p2 ack must still land (delivery was never consumed): %v", err)
	}
}
