package relay

import (
	"testing"

	"agent-relay/internal/db"
)

// A backlog dispatch is groomed-but-silent: it must NOT deliver the claim
// notification to the profile's agents (no wake, not picked up). promote_task
// then announces it exactly like a fresh dispatch, so the delivery lands.
func TestBacklogDispatch_SkipsNotifyUntilPromote(t *testing.T) {
	h := testHandlers(t)
	prof := "dev"
	if _, _, err := h.db.RegisterAgent("p1", "w1", "worker", "", nil, &prof, false, nil, "[]", 0, db.RegisterOptions{ProfileSlugSet: true}); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "groomed", "backlog": true,
	}))
	task := parseJSON(t, res)["task"].(map[string]any)
	if task["status"] != "backlog" {
		t.Fatalf("status = %v, want backlog", task["status"])
	}
	taskID := task["id"].(string)

	if n, _ := h.db.UnreadCountForAgent("p1", "w1"); n != 0 {
		t.Fatalf("backlog dispatch must not notify the profile, got %d unread", n)
	}

	// Promote → pending; now the worker is notified.
	h.HandlePromoteTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": taskID}))
	if n, _ := h.db.UnreadCountForAgent("p1", "w1"); n != 1 {
		t.Fatalf("promote must notify the profile, got %d unread", n)
	}

	// Double-promote (task already pending) must be an idempotent no-op — no second
	// wake/delivery (review-121f0ff5 finding).
	h.HandlePromoteTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": taskID}))
	if n, _ := h.db.UnreadCountForAgent("p1", "w1"); n != 1 {
		t.Fatalf("double-promote must not re-notify, got %d unread", n)
	}
}
