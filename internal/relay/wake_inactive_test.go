package relay

import "testing"

// Bug 6509668c regression: a dispatch aimed at a profile whose only worker was
// swept to 'inactive' (the 30-min idle sweep) must still land a DURABLE queued
// delivery for that worker, so niwa's unread-count poll can wake + re-register it.
// Before the fix, GetAgentsByProfile filtered status='active' (the inactive worker
// dropped out of the fan-out) AND the fan-out used InsertMessage (no delivery row
// even for an active worker) — so UnreadCountForAgent read 0 and the work stranded.
func TestDispatchWakesInactiveProfileAgent(t *testing.T) {
	h := testHandlers(t)

	// Dispatcher + a worker on the 'dev' profile.
	if _, err := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "cto", "role": "lead"})); err != nil {
		t.Fatalf("register cto: %v", err)
	}
	if _, err := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "worker", "role": "dev", "profile_slug": "dev"})); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	// The worker goes idle → swept inactive.
	if err := h.db.DeactivateAgent("p1", "worker"); err != nil {
		t.Fatalf("deactivate worker: %v", err)
	}

	// Dispatch to the 'dev' profile.
	res, err := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "wake me",
	}))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	parseJSON(t, res) // fail loudly if the dispatch itself errored

	// The inactive worker must have a durable, pollable wake signal.
	n, err := h.db.UnreadCountForAgent("p1", "worker")
	if err != nil {
		t.Fatalf("unread count: %v", err)
	}
	if n < 1 {
		t.Fatalf("inactive profile-agent must receive a durable dispatch delivery, got unread=%d", n)
	}

	// And the task notification actually surfaces in its inbox.
	msgs, err := h.db.GetInbox("p1", "worker", true, 50)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.Type == "task" {
			found = true
			break
		}
	}
	if !found {
		t.Error("dispatch task message must surface in the inactive worker's inbox")
	}
}
