package relay

import (
	"net/http"
	"testing"

	"agent-relay/internal/db"
)

// S1 (audit b983684b §1): DELETE /api/agents/:name tombstones a known agent
// (status='deleted') and 404s an unknown one.
func TestAPIDeleteAgent(t *testing.T) {
	r := testRelay(t)
	if _, _, err := r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	w := doAPI(r, http.MethodDelete, "/agents/bot-a?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["deleted"]; got != true {
		t.Errorf("delete: want deleted=true, got %v", got)
	}
	a, err := r.DB.GetAgent("p1", "bot-a")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if a == nil || a.Status != "deleted" {
		t.Errorf("agent status: want deleted, got %+v", a)
	}

	// Unknown agent -> 404 with the standard error envelope.
	w = doAPI(r, http.MethodDelete, "/agents/ghost?project=p1", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: want 404, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["error"]; got != "agent not found" {
		t.Errorf("delete unknown: want error 'agent not found', got %v", got)
	}
}

// S1 (audit b983684b §1): POST /api/agents/:name/deactivate flips a known
// agent to inactive and 404s an unknown one.
func TestAPIDeactivateAgent(t *testing.T) {
	r := testRelay(t)
	if _, _, err := r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	w := doAPI(r, http.MethodPost, "/agents/bot-a/deactivate?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("deactivate: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["deactivated"]; got != true {
		t.Errorf("deactivate: want deactivated=true, got %v", got)
	}
	a, err := r.DB.GetAgent("p1", "bot-a")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if a == nil || a.Status != "inactive" {
		t.Errorf("agent status: want inactive, got %+v", a)
	}

	// Unknown agent -> 404 with the standard error envelope.
	w = doAPI(r, http.MethodPost, "/agents/ghost/deactivate?project=p1", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("deactivate unknown: want 404, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["error"]; got != "agent not found" {
		t.Errorf("deactivate unknown: want error 'agent not found', got %v", got)
	}
}
