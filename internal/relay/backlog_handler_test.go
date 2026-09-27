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
	_, _ = h.HandlePromoteTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": taskID}))
	if n, _ := h.db.UnreadCountForAgent("p1", "w1"); n != 1 {
		t.Fatalf("promote must notify the profile, got %d unread", n)
	}

	// Double-promote (task already pending) must be an idempotent no-op — no second
	// wake/delivery (review-121f0ff5 finding).
	_, _ = h.HandlePromoteTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": taskID}))
	if n, _ := h.db.UnreadCountForAgent("p1", "w1"); n != 1 {
		t.Fatalf("double-promote must not re-notify, got %d unread", n)
	}
}

// batch_dispatch_tasks honours per-item backlog:true: each item behaves as its
// single dispatch_task twin, so a backlog item is silent (no delivery, no
// task.dispatched) until promoted and a pending item in the same batch notifies.
func TestBatchDispatch_Backlog(t *testing.T) {
	setup := func(t *testing.T) *Handlers {
		t.Helper()
		h := testHandlers(t)
		prof := "dev"
		if _, _, err := h.db.RegisterAgent("p1", "w1", "worker", "", nil, &prof, false, nil, "[]", 0, db.RegisterOptions{ProfileSlugSet: true}); err != nil {
			t.Fatalf("register worker: %v", err)
		}
		return h
	}
	batch := func(t *testing.T, h *Handlers, tasks string) map[string]string {
		t.Helper()
		res, _ := h.HandleBatchDispatchTasks(ctx, call(map[string]any{"project": "p1", "as": "cto", "tasks": tasks}))
		body := parseJSON(t, res)
		if errs, _ := body["errors"].([]any); len(errs) != 0 {
			t.Fatalf("batch errors: %v", errs)
		}
		ids := map[string]string{}
		for _, d := range body["dispatched"].([]any) {
			m := d.(map[string]any)
			ids[m["title"].(string)] = m["id"].(string)
		}
		return ids
	}
	status := func(t *testing.T, h *Handlers, id string) string {
		t.Helper()
		task, err := h.db.GetTask(id, "p1")
		if err != nil {
			t.Fatalf("get task %s: %v", id, err)
		}
		return task.Status
	}
	dispatched := func(h *Handlers, id string) int {
		n := 0
		for _, ev := range h.events.Recent("p1", 0) {
			if ev.Type == "task.dispatched" && ev.Semantic["task_id"] == id {
				n++
			}
		}
		return n
	}
	unread := func(t *testing.T, h *Handlers) int {
		t.Helper()
		n, err := h.db.UnreadCountForAgent("p1", "w1")
		if err != nil {
			t.Fatalf("unread: %v", err)
		}
		return n
	}

	t.Run("SkipsNotifyUntilPromote", func(t *testing.T) {
		h := setup(t)
		id := batch(t, h, `[{"profile":"dev","title":"groomed","backlog":true}]`)["groomed"]
		if s := status(t, h, id); s != "backlog" {
			t.Fatalf("status = %q, want backlog", s)
		}
		if n := unread(t, h); n != 0 {
			t.Fatalf("backlog batch item must not notify the profile, got %d unread", n)
		}
		if n := dispatched(h, id); n != 0 {
			t.Fatalf("backlog batch item emitted %d task.dispatched, want 0", n)
		}

		_, _ = h.HandlePromoteTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": id}))
		if n := unread(t, h); n != 1 {
			t.Fatalf("promote must notify the profile, got %d unread", n)
		}
		if n := dispatched(h, id); n != 1 {
			t.Fatalf("promoted item emitted %d task.dispatched, want 1", n)
		}
	})

	t.Run("MixedBatchPerItem", func(t *testing.T) {
		h := setup(t)
		ids := batch(t, h, `[{"profile":"dev","title":"later","backlog":true},{"profile":"dev","title":"now"}]`)
		if s := status(t, h, ids["later"]); s != "backlog" {
			t.Fatalf("backlog item status = %q, want backlog", s)
		}
		if s := status(t, h, ids["now"]); s != "pending" {
			t.Fatalf("plain item status = %q, want pending", s)
		}
		if n := unread(t, h); n != 1 {
			t.Fatalf("only the pending item notifies the profile, got %d unread", n)
		}
		if n := dispatched(h, ids["later"]); n != 0 {
			t.Fatalf("backlog item emitted %d task.dispatched, want 0", n)
		}
		if n := dispatched(h, ids["now"]); n != 1 {
			t.Fatalf("pending item emitted %d task.dispatched, want 1", n)
		}
	})
}
