package relay

import (
	"testing"
	"time"
)

// The wake daemon (agentd sse.rs) gates on the message kind straight off the
// SSE envelope, so the relay must stamp MsgType onto the "message" MCPEvent it
// emits. Regress this and every ack wakes a busy agent again.
func TestSendMessage_EmitsMsgType(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "dev"}))

	cases := []struct {
		name    string
		typeArg any
		want    string
	}{
		{"ack is carried", "ack", "ack"},
		{"fyi is carried", "fyi", "fyi"},
		{"absent type defaults to notification", nil, "notification"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := h.events.Subscribe()
			defer h.events.Unsubscribe(sub)

			args := map[string]any{"project": "p1", "as": "bot-a", "to": "bot-b", "content": "hi"}
			if tc.typeArg != nil {
				args["type"] = tc.typeArg
			}
			if _, err := h.HandleSendMessage(ctx, call(args)); err != nil {
				t.Fatalf("send: %v", err)
			}

			evt := waitForMessageEvent(t, sub)
			if evt.MsgType != tc.want {
				t.Errorf("MsgType = %q, want %q (full: %+v)", evt.MsgType, tc.want, evt)
			}
		})
	}
}

// waitForMessageEvent drains the bus until the Type:"message" event (register
// and other bus traffic can arrive first) or fails on timeout.
func waitForMessageEvent(t *testing.T, sub chan MCPEvent) MCPEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-sub:
			if e.Type == "message" {
				return e
			}
		case <-deadline:
			t.Fatal("no Type:\"message\" event fired")
		}
	}
}

// Task cee47c61 round 2: the "message" MCPEvent is this same wake channel —
// agentd gates on it straight off the SSE envelope (no message_id to dedup
// on), so a dedup-hit retry must not emit a second one either, on top of the
// registry.Notify suppression.
func TestSendMessage_DedupHitSuppressesMessageEvent(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a", "role": "dev"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "role": "dev"}))

	if _, err := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "retry me",
		"idempotency_key": "dedup-event-1",
	})); err != nil {
		t.Fatalf("first send: %v", err)
	}

	sub := h.events.Subscribe()
	defer h.events.Unsubscribe(sub)

	if _, err := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "content": "retry me",
		"idempotency_key": "dedup-event-1",
	})); err != nil {
		t.Fatalf("dedup-hit retry: %v", err)
	}

	select {
	case e := <-sub:
		if e.Type == "message" {
			t.Fatalf("dedup-hit retry must not emit a second Type:\"message\" event, got %+v", e)
		}
	case <-time.After(300 * time.Millisecond):
		// no message event — correct.
	}
}
