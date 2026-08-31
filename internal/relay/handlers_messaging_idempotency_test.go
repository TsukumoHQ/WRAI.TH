package relay

import "testing"

// Task ac328091: send_message accepts an optional idempotency_key. Two calls
// with the same key return the same message id and do not duplicate the
// recipient's inbox — a genuine client retry (e.g. after a timeout) no longer
// produces a second delivery.
func TestSendMessage_SameIdempotencyKeyReturnsSameMessageNoDuplicateDelivery(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b"}))

	res1, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "retry me",
		"idempotency_key": "client-retry-1",
	}))
	msg1 := parseJSON(t, res1)
	id1, _ := msg1["id"].(string)
	if id1 == "" {
		t.Fatalf("expected message id in first response, got %v", msg1)
	}

	res2, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "retry me",
		"idempotency_key": "client-retry-1",
	}))
	msg2 := parseJSON(t, res2)
	id2, _ := msg2["id"].(string)
	if id2 != id1 {
		t.Fatalf("retry with same idempotency_key should return same message id, got %s then %s", id1, id2)
	}

	inboxRes, _ := h.HandleGetInbox(ctx, call(map[string]any{
		"format": "json", "project": "p1", "as": "bot-b", "unread_only": true,
	}))
	msgs := parseJSON(t, inboxRes)["messages"].([]any)
	count := 0
	for _, mm := range msgs {
		m := mm.(map[string]any)
		if m["id"] == id1 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 inbox entry for the retried message, got %d", count)
	}
}

// A different (or absent) idempotency_key must never dedup — this is what
// keeps legitimate identical-content resends (heartbeats, repeated status)
// unaffected.
func TestSendMessage_NoOrDifferentIdempotencyKeyCreatesNewMessage(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b"}))

	res1, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "status: still working",
	}))
	res2, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "status: still working",
	}))
	id1 := parseJSON(t, res1)["id"].(string)
	id2 := parseJSON(t, res2)["id"].(string)
	if id1 == id2 {
		t.Fatalf("identical-content resends without a key must never dedup, got same id %s", id1)
	}
}
