package relay

import (
	"net/http"
	"strings"
	"testing"
)

func TestApiDecisionGraphAndRelevant(t *testing.T) {
	r := testRelay(t)
	base, err := r.DB.RememberDecision("default", "a", "relay/wake", "guard-first wake", "", nil, "", nil)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	if _, err := r.DB.RememberDecision("default", "a", "relay/metrics", "no-wake share metric", "", nil, "", []string{base.Key}); err != nil {
		t.Fatalf("dep: %v", err)
	}

	// Graph endpoint returns mermaid.
	w := doAPI(r, "GET", "/decisions/graph", "")
	if w.Code != http.StatusOK {
		t.Fatalf("graph status = %d; body=%s", w.Code, w.Body.String())
	}
	out := decodeJSON(t, w)
	if mm, _ := out["mermaid"].(string); !strings.Contains(mm, "graph TD") || !strings.Contains(mm, "-.->|depends|") {
		t.Errorf("mermaid payload missing content: %v", out["mermaid"])
	}

	// Relevant endpoint filters by area.
	w2 := doAPI(r, "GET", "/decisions/relevant?area=relay/wake", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("relevant status = %d", w2.Code)
	}
	out2 := decodeJSON(t, w2)
	if c, _ := out2["count"].(float64); c != 1 {
		t.Errorf("relevant count = %v, want 1", out2["count"])
	}
}
