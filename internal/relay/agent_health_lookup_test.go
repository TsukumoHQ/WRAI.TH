package relay

import (
	"net/http"
	"testing"

	"agent-relay/internal/db"
)

// TestAgentHealthSingleLookup: task 6c1c5167 (niwa ledger-seam liveness read,
// contract with niwa-cto). GET /agents/health?project=&agent=<name> returns
// the single AgentHealth object (not an array) for that agent, 404 for a
// name that doesn't exist, and leaves the existing whole-project array
// response (no ?agent=) unchanged.
func TestAgentHealthSingleLookup(t *testing.T) {
	r := testRelay(t)
	const project = "p1"

	if _, _, err := r.DB.RegisterAgent(project, "worker", "", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}

	// Single-agent lookup: 200 + one object with the liveness fields the seam
	// reads (last_seen, idle_seconds, status, as_of).
	w := doAPI(r, "GET", "/agents/health?project="+project+"&agent=worker", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeJSON(t, w)
	if got["agent"] != "worker" {
		t.Fatalf("agent = %v, want worker", got["agent"])
	}
	for _, field := range []string{"status", "last_seen", "idle_seconds", "as_of"} {
		if _, ok := got[field]; !ok {
			t.Fatalf("missing field %q in single-agent response: %v", field, got)
		}
	}

	// Unknown agent: 404, not a fabricated "dead" record.
	w = doAPI(r, "GET", "/agents/health?project="+project+"&agent=nobody", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown agent, got %d: %s", w.Code, w.Body.String())
	}

	// No ?agent=: whole-project array, unchanged shape.
	w = doAPI(r, "GET", "/agents/health?project="+project, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	arr := decodeJSONArray(t, w)
	if len(arr) != 1 {
		t.Fatalf("expected 1 agent in whole-project response, got %d", len(arr))
	}
}
