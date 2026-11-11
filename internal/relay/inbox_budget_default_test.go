package relay

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// Audit T5 (task 6ead6bed) AC3: get_inbox applies the context budget by default
// for an agent that has max_context_bytes set, an explicit apply_budget always
// wins, and pruning never consumes a message (non-destructive inbox).

// inboxIDs fetches bot-b's unread inbox with extra args and returns the ids.
func inboxIDs(t *testing.T, h *Handlers, extra map[string]any) []string {
	t.Helper()
	args := map[string]any{"format": "json", "project": "p1", "as": "bot-b", "unread_only": true, "limit": 50}
	for k, v := range extra {
		args[k] = v
	}
	res, err := h.HandleGetInbox(ctx, call(args))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, mm := range parseJSON(t, res)["messages"].([]any) {
		ids = append(ids, mm.(map[string]any)["id"].(string))
	}
	return ids
}

// budgetFixture registers bot-a and bot-b (bot-b with maxBytes budget) and
// sends n P2 messages of size bytes from bot-a to bot-b.
func budgetFixture(t *testing.T, maxBytes, n, size int) *Handlers {
	t.Helper()
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b", "max_context_bytes": maxBytes}))
	for i := 0; i < n; i++ {
		res, _ := h.HandleSendMessage(ctx, call(map[string]any{
			"project": "p1", "as": "bot-a", "to": "bot-b", "content": strings.Repeat("x", size),
		}))
		parseJSON(t, res)
	}
	return h
}

func TestInboxBudget_DefaultOnPrunesOverBudget(t *testing.T) {
	// 6 messages of ~600B against a 2000B budget: at most 3 fit.
	h := budgetFixture(t, 2000, 6, 600)

	got := inboxIDs(t, h, nil)
	if len(got) == 0 || len(got) >= 6 {
		t.Fatalf("default get_inbox should prune to the budget: got %d of 6", len(got))
	}
	if len(got) > 3 {
		t.Fatalf("budget 2000B admits at most 3 x 600B messages, got %d", len(got))
	}

	// The pruning is never silent: JSON carries held_back + a hint, the
	// default markdown says it in its header line.
	res, _ := h.HandleGetInbox(ctx, call(map[string]any{"format": "json", "project": "p1", "as": "bot-b", "unread_only": true, "limit": 50}))
	data := parseJSON(t, res)
	if hb, _ := data["held_back"].(float64); int(hb) != 6-len(got) {
		t.Fatalf("held_back = %v, want %d", data["held_back"], 6-len(got))
	}
	if data["held_back_hint"] == nil {
		t.Fatal("held_back without a hint")
	}
	res, _ = h.HandleGetInbox(ctx, call(map[string]any{"project": "p1", "as": "bot-b", "unread_only": true, "limit": 50}))
	md := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(strings.SplitN(md, "\n", 2)[0], "held back by the context budget") {
		t.Fatalf("markdown header must say messages were held back: %q", strings.SplitN(md, "\n", 2)[0])
	}
}

func TestInboxBudget_ExplicitFalseWins(t *testing.T) {
	h := budgetFixture(t, 2000, 6, 600)
	if got := inboxIDs(t, h, map[string]any{"apply_budget": false}); len(got) != 6 {
		t.Fatalf("apply_budget=false must return the whole inbox, got %d of 6", len(got))
	}
}

// TestInboxBudget_PrunedStayUnread is the non-destructive guarantee: messages
// the default budget dropped are neither surfaced nor read — they stay in the
// unread inbox and come through once the survivors are marked read.
func TestInboxBudget_PrunedStayUnread(t *testing.T) {
	h := budgetFixture(t, 2000, 6, 600)

	first := inboxIDs(t, h, nil)
	if len(first) >= 6 {
		t.Fatalf("precondition: default poll should prune, got %d of 6", len(first))
	}
	// A pruned poll must not lose anything: the full view still has all 6.
	if all := inboxIDs(t, h, map[string]any{"apply_budget": false}); len(all) != 6 {
		t.Fatalf("pruned messages vanished from the unread inbox: %d of 6 left", len(all))
	}

	// Drain survivors poll by poll; every message must eventually surface.
	seen := map[string]bool{}
	batch := first
	for round := 0; len(batch) > 0 && round < 10; round++ {
		ids := make([]any, len(batch))
		for i, id := range batch {
			if seen[id] {
				t.Fatalf("message %s returned again after mark_read", id)
			}
			seen[id] = true
			ids[i] = id
		}
		_, _ = h.HandleMarkRead(ctx, call(map[string]any{"project": "p1", "as": "bot-b", "message_ids": ids}))
		batch = inboxIDs(t, h, nil)
	}
	if len(seen) != 6 {
		t.Fatalf("budgeted polls delivered %d of 6 messages; pruned ones were lost", len(seen))
	}
}

func TestInboxBudget_FitsReturnsAll(t *testing.T) {
	// Column default budget (16384) with a small inbox: nothing pruned.
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-b"}))
	for i := 0; i < 3; i++ {
		_, _ = h.HandleSendMessage(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "to": "bot-b", "content": "hi"}))
	}
	if got := inboxIDs(t, h, nil); len(got) != 3 {
		t.Fatalf("an inbox under budget must be returned whole, got %d of 3", len(got))
	}
	res, _ := h.HandleGetInbox(ctx, call(map[string]any{"format": "json", "project": "p1", "as": "bot-b"}))
	if _, ok := parseJSON(t, res)["held_back"]; ok {
		t.Fatal("held_back must be absent when nothing was pruned")
	}
}

func TestInboxBudget_P0NeverPruned(t *testing.T) {
	// The P0 alone exceeds the budget; it must still be delivered.
	h := budgetFixture(t, 500, 3, 600)
	res, _ := h.HandleSendMessage(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "to": "bot-b", "priority": "P0", "content": strings.Repeat("!", 900),
	}))
	p0 := parseJSON(t, res)["id"].(string)

	got := inboxIDs(t, h, nil)
	found := false
	for _, id := range got {
		if id == p0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("P0 pruned by the default budget: got %v", got)
	}
}

func TestInboxBudget_ZeroBudgetDisablesDefault(t *testing.T) {
	// max_context_bytes=0 means no budget: the default stays off.
	h := budgetFixture(t, 0, 6, 600)
	if got := inboxIDs(t, h, nil); len(got) != 6 {
		t.Fatalf("agent without a budget must get the whole inbox, got %d of 6", len(got))
	}
}
