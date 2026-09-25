package relay

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/models"
)

// Task adefc1f4: a type=policy message never expires, is never deadlettered,
// and stays in the recipient's session_context until its delivery is acked.

func policySetup(t *testing.T) *Handlers {
	t.Helper()
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "dev"}))
	return h
}

func sendPolicy(t *testing.T, h *Handlers, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"project": "p1", "as": "cto", "to": "dev", "type": "policy", "subject": "POLICY: typed tickets", "content": "every ticket is typed and tested"}
	for k, v := range extra {
		args[k] = v
	}
	res, _ := h.HandleSendMessage(ctx, call(args))
	return parseJSON(t, res)["id"].(string)
}

func bootPolicies(t *testing.T, h *Handlers, agent string) ([]MessageSummary, []MessageSummary) {
	t.Helper()
	sc := h.buildSessionContext("p1", agent, nil)
	unread, _ := sc["unread_messages"].([]MessageSummary)
	policies, _ := sc["policies"].([]MessageSummary)
	return unread, policies
}

func TestPolicy(t *testing.T) {
	t.Run("PolicyTTLForcedZero", func(t *testing.T) {
		h := policySetup(t)
		id := sendPolicy(t, h, map[string]any{"ttl_seconds": 60, "priority": "P2"})
		msg, err := h.db.GetMessage(id)
		if err != nil || msg == nil {
			t.Fatalf("get message: %v", err)
		}
		if msg.TTLSeconds != 0 {
			t.Fatalf("policy ttl = %d, want 0 whatever the caller passed", msg.TTLSeconds)
		}
	})

	t.Run("PolicyNeverDeadlettered", func(t *testing.T) {
		h := policySetup(t)
		id := sendPolicy(t, h, map[string]any{"ttl_seconds": 1})
		plain := parseJSON(t, mustSend(t, h, map[string]any{"project": "p1", "as": "cto", "to": "dev", "subject": "s", "content": "fyi", "ttl_seconds": 1}))["id"].(string)
		time.Sleep(2100 * time.Millisecond) // past both 1s TTLs at datetime()'s second granularity
		if _, err := h.db.ExpireMessages(); err != nil {
			t.Fatalf("expire messages: %v", err)
		}
		if _, err := h.db.ExpireDeliveries(); err != nil {
			t.Fatalf("expire deliveries: %v", err)
		}
		if _, err := h.db.PurgeExpiredMessages(0); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if msg, _ := h.db.GetMessage(id); msg == nil || msg.ExpiredAt != nil {
			t.Fatalf("policy message expired or purged: %+v", msg)
		}
		rows, err := h.db.Deadletter("p1", "dev", 50)
		if err != nil {
			t.Fatalf("deadletter: %v", err)
		}
		sawPlain := false
		for _, r := range rows {
			if r.MessageID == id {
				t.Fatalf("policy message was deadlettered")
			}
			if r.MessageID == plain {
				sawPlain = true
			}
		}
		if !sawPlain {
			t.Fatalf("control: the non-policy message should have been deadlettered")
		}
	})

	t.Run("PolicyDefaultsToDo", func(t *testing.T) {
		h := policySetup(t)
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{"project": "p1", "as": "cto", "to": "dev", "type": "policy", "subject": "s", "content": "c"}))
		if got := parseJSON(t, res)["action_required"]; got != "do" {
			t.Fatalf("policy action_required = %v, want do", got)
		}
	})

	t.Run("UnackedPolicySurfacedAtBoot", func(t *testing.T) {
		h := policySetup(t)
		id := sendPolicy(t, h, nil)
		// get_inbox surfaces (not acks) it: it must still show at the next boot.
		_, _ = h.HandleGetInbox(ctx, call(map[string]any{"project": "p1", "as": "dev", "unread_only": true}))
		unread, policies := bootPolicies(t, h, "dev")
		if len(policies) != 1 || policies[0].ID != id {
			t.Fatalf("policies block = %+v, want the unacked policy %s", policies, id)
		}
		for _, m := range unread {
			if m.ID == id {
				t.Fatalf("policy duplicated into unread_messages")
			}
		}
	})

	t.Run("AckedPolicyNotSurfaced", func(t *testing.T) {
		h := policySetup(t)
		id := sendPolicy(t, h, nil)
		res, _ := h.HandleMarkRead(ctx, call(map[string]any{"project": "p1", "as": "dev", "message_ids": []any{id}}))
		if res.IsError {
			t.Fatalf("mark_read: %s", expectError(t, res))
		}
		sc := h.buildSessionContext("p1", "dev", nil)
		if _, ok := sc["policies"]; ok {
			t.Fatalf("acked policy still surfaced: %v", sc["policies"])
		}
	})

	t.Run("PolicyBlockRespectsHardCeiling", func(t *testing.T) {
		var msgs []models.Message
		for i := 0; i < 20; i++ {
			msgs = append(msgs, models.Message{ID: fmt.Sprintf("pol-%02d", i), From: "cto", Type: "policy", Priority: "P2", Subject: "policy", Content: strings.Repeat("x", 400)})
		}
		room := 1500
		out := projectPolicies(msgs, room, "dev")
		used := 0
		for _, s := range out {
			used += messageSummaryBytes(s)
		}
		if used > room {
			t.Fatalf("policy block used %d bytes, room %d", used, room)
		}
		if len(out) == 0 || len(out) == len(msgs) {
			t.Fatalf("expected a partial block, got %d of %d", len(out), len(msgs))
		}
		// With no room left, the first policy still surfaces (like the first P0).
		if first := projectPolicies(msgs, 0, "dev"); len(first) != 1 {
			t.Fatalf("zero room: got %d policies, want exactly the first", len(first))
		}
	})

	t.Run("JournalMarksPolicyItems", func(t *testing.T) {
		h := policySetup(t)
		id := sendPolicy(t, h, nil)
		lines := captureBudgetJournal(t, func() { h.buildSessionContext("p1", "dev", nil) })
		found := false
		for _, j := range lines {
			if j.Path == "session_context_policy" {
				if j.Agent != "dev" || len(j.Selected) != 1 || j.Selected[0] != id {
					t.Fatalf("policy journal line = %+v", j)
				}
				found = true
			}
			if j.Path == "session_context" {
				for _, c := range j.Candidates {
					if c == id {
						t.Fatalf("policy item leaked into the soft-budget journal")
					}
				}
			}
		}
		if !found {
			t.Fatalf("no session_context_policy journal line")
		}
	})

	t.Run("NonPolicyBootUnchanged", func(t *testing.T) {
		h := policySetup(t)
		_ = mustSend(t, h, map[string]any{"project": "p1", "as": "cto", "to": "dev", "subject": "s", "content": "plain"})
		unread, _ := bootPolicies(t, h, "dev")
		sc := h.buildSessionContext("p1", "dev", nil)
		if _, ok := sc["policies"]; ok {
			t.Fatalf("policies block present with no policy message")
		}
		if len(unread) != 1 {
			t.Fatalf("unread_messages = %d, want 1", len(unread))
		}
	})
}
