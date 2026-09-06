package relay

import (
	"testing"

	"agent-relay/internal/db"
)

// AC4: a direct eligibility read of a soft-deleted agent surfaces an explicit
// "deleted" state (loud), never a silent not-found ambiguity. The reason must be
// distinct from "unregistered" so a client can tell a killed agent apart from a
// never-existed one and PARK rather than hot-loop.
func TestIsEligible_DeletedAgentLoudReason(t *testing.T) {
	h := testHandlers(t)
	const proj = "test-proj"

	if _, _, err := h.db.RegisterAgent(proj, "ghost", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := h.db.DeleteAgent(proj, "ghost"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	res, err := h.HandleIsEligible(ctx, call(map[string]any{"project": proj, "agent": "ghost"}))
	if err != nil {
		t.Fatalf("is_eligible: %v", err)
	}
	data := parseJSON(t, res)
	if data["eligible"] != false {
		t.Errorf("deleted agent eligible = %v, want false", data["eligible"])
	}
	if data["reason"] != "deleted" {
		t.Errorf("deleted agent reason = %q, want loud %q", data["reason"], "deleted")
	}

	// A never-registered name reports "unregistered" — proving "deleted" is a
	// distinct, non-silent verdict, not collapsed into a generic not-found.
	res2, err := h.HandleIsEligible(ctx, call(map[string]any{"project": proj, "agent": "nobody"}))
	if err != nil {
		t.Fatalf("is_eligible nobody: %v", err)
	}
	data2 := parseJSON(t, res2)
	if data2["reason"] != "unregistered" {
		t.Errorf("unregistered agent reason = %q, want %q", data2["reason"], "unregistered")
	}
}
