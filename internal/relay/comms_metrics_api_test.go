package relay

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestApiGetCommsMetrics(t *testing.T) {
	r := testRelay(t)
	// A wake (task) and a no-wake (notification) in project p.
	if _, _, err := r.DB.InsertMessageWithDeliveries("p", "cto", "b", "task", "s", "x", "{}", "P2", 3600, nil, nil, []string{"b"}, ""); err != nil {
		t.Fatalf("task: %v", err)
	}
	if _, _, err := r.DB.InsertMessageWithDeliveries("p", "cto", "b", "notification", "s", "x", "{}", "P2", 3600, nil, nil, []string{"b"}, ""); err != nil {
		t.Fatalf("notif: %v", err)
	}

	w := doAPI(r, "GET", "/metrics/comms?project=p&hours=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("comms metrics not valid JSON: %v", err)
	}
	for _, key := range []string{"project", "target_no_wake_share", "deliveries", "action_required_by_sender", "read_latency", "coalescing_candidates"} {
		if _, ok := out[key]; !ok {
			t.Errorf("comms metrics missing %q", key)
		}
	}
	dv, _ := out["deliveries"].(map[string]any)
	if got, _ := dv["total"].(float64); got != 2 {
		t.Errorf("deliveries.total = %v, want 2", dv["total"])
	}
	if got, _ := dv["no_wake"].(float64); got != 1 {
		t.Errorf("deliveries.no_wake = %v, want 1", dv["no_wake"])
	}
}
