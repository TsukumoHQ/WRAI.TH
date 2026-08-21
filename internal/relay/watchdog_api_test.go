package relay

import (
	"fmt"
	"net/http"
	"testing"

	"agent-relay/internal/db"
)

// TestWatchdogEndpoints wires the two liveness-watchdog routes end to end:
// GET /agents/stuck returns the candidate set (JSON array), and
// POST /tasks/{id}/requeue releases a held task back to pending so a fresh agent
// can claim it. (The idle-threshold detection itself is exercised in the DB-layer
// TestStuckAgents; here we prove the HTTP wiring + serialization.)
func TestWatchdogEndpoints(t *testing.T) {
	r := testRelay(t)
	const project = "p1"

	if _, _, err := r.DB.RegisterAgent(project, "stuck", "", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	task, err := r.DB.DispatchTask(project, "prof", "lead", "held work", "", "P2", nil, nil, db.TypedTicket{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB.ClaimTask(task.ID, "stuck", project); err != nil {
		t.Fatal(err)
	}

	// The read returns 200 + a JSON array (a fresh agent is not yet stuck).
	w := doAPI(r, "GET", "/agents/stuck?project="+project, "")
	if w.Code != http.StatusOK {
		t.Fatalf("stuck: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if arr := decodeJSONArray(t, w); len(arr) != 0 {
		t.Fatalf("fresh agent must not be stuck, got %d", len(arr))
	}

	// Requeue the held task → pending.
	body := fmt.Sprintf(`{"project":%q,"reason":"test"}`, project)
	w = doAPI(r, "POST", "/tasks/"+task.ID+"/requeue", body)
	if w.Code != http.StatusOK {
		t.Fatalf("requeue: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if out := decodeJSON(t, w); out["status"] != "pending" {
		t.Fatalf("expected pending after requeue, got %v", out["status"])
	}

	// A fresh agent can now claim the requeued work.
	if _, err := r.DB.ClaimTask(task.ID, "rescuer", project); err != nil {
		t.Fatalf("re-claim after requeue: %v", err)
	}
}
