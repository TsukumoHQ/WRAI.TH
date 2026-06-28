package relay

import (
	"encoding/json"
	"net/http"
	"testing"

	"agent-relay/internal/db"
)

func TestApiGetMetrics(t *testing.T) {
	r := testRelay(t)
	if _, _, err := r.DB.RegisterAgent("default", "a0", "m", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	w := doAPI(r, "GET", "/metrics", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("metrics not valid JSON: %v", err)
	}
	for _, key := range []string{"version", "uptime_seconds", "agents", "messages", "deliveries", "tasks", "db"} {
		if _, ok := out[key]; !ok {
			t.Errorf("metrics missing %q", key)
		}
	}
	agents, ok := out["agents"].(map[string]any)
	if !ok {
		t.Fatalf("agents block malformed: %T", out["agents"])
	}
	// One registered agent → active count 1 (JSON numbers decode to float64).
	if got, _ := agents["active"].(float64); got != 1 {
		t.Errorf("agents.active = %v, want 1", agents["active"])
	}
}
