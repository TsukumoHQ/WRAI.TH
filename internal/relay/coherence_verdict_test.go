package relay

import (
	"strings"
	"testing"
	"time"

	"agent-relay/internal/db"
)

// TestCoherenceVerdicts drives the verdict routing of obligation_discharge on
// coherence obligations end to end (design e731f3c9 §4.2, §5, §6): conflict
// needs a reason and opens a contest on the reviewer, upheld re-opens the
// raiser's reassess, and an advisory claim carries stale_context.
func TestCoherenceVerdicts(t *testing.T) {
	h := testHandlers(t)
	lead, dev := "lead", "dev"
	for _, a := range []struct {
		name    string
		reports *string
		profile *string
	}{{"lead", nil, nil}, {"cto", &lead, nil}, {"a", &lead, &dev}} {
		if _, _, err := h.db.RegisterAgent("p1", a.name, "dev", "", a.reports, a.profile, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
			t.Fatalf("register %s: %v", a.name, err)
		}
	}
	v1, err := h.db.SetMemory("p1", "cto", "auth-policy", "jwt allowed", "", "project", "", "constraints")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.db.RecordSnapshot("p1", "a", "", db.SnapshotBoot, []string{v1.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SetMemory("p1", "cto", "auth-policy", "jwt banned", "", "project", "", "constraints"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.EvaluateCoherence(time.Now()); err != nil {
		t.Fatal(err)
	}
	mine := func(agent string) string {
		obs, err := h.db.MyCoherenceObligations("p1", agent)
		if err != nil || len(obs) != 1 {
			t.Fatalf("%s obligations = %d (%v), want 1", agent, len(obs), err)
		}
		return obs[0].ID
	}
	id := mine("a")

	// Advisory: the claim stands and carries the stale list.
	task, err := h.db.DispatchTask("p1", "dev", "lead", "unrelated work", "", "P2", nil, nil, db.TypedTicket{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "a", "task_id": task.ID}))
	if res.IsError {
		t.Fatalf("advisory claim refused: %s", expectError(t, res))
	}
	if stale, ok := parseJSON(t, res)["stale_context"].([]any); !ok || len(stale) != 1 {
		t.Fatalf("claim result stale_context = %v, want one entry", parseJSON(t, res)["stale_context"])
	}

	res, _ = h.HandleObligationDischarge(ctx, call(map[string]any{"project": "p1", "as": "a", "id": id, "verdict": "conflict"}))
	if msg := expectError(t, res); !strings.Contains(msg, "reason") {
		t.Fatalf("conflict without reason: %s", msg)
	}
	res, _ = h.HandleObligationDischarge(ctx, call(map[string]any{"project": "p1", "as": "a", "id": id,
		"verdict": "conflict", "reason": "mobile clients still need jwt"}))
	out := parseJSON(t, res)
	if out["state"] != db.ObligationFulfilled || out["contest_id"] == nil || out["verdict"] != "conflict" {
		t.Fatalf("conflict result = %v", out)
	}
	review := mine("lead")
	res, _ = h.HandleObligationDischarge(ctx, call(map[string]any{"project": "p1", "as": "lead", "id": review, "verdict": "upheld"}))
	if res.IsError {
		t.Fatalf("upheld refused: %s", expectError(t, res))
	}
	if again := mine("a"); again == id {
		t.Fatal("upheld did not re-open the raiser's reassess")
	}
}
