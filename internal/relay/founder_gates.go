package relay

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"agent-relay/internal/db"
)

// evaluateFounderGates opens a gate for every pending task that entered a
// founder gate (profile human/user, or parked until the founder) and alerts
// the founder once per entry: the alerted_at CAS lets exactly one caller (the
// ACK tick or the park_task handler) send it. P1 "do", durable, pushed live.
func evaluateFounderGates(database *db.DB, notifier ackNotifier, now time.Time) {
	gates, err := database.SyncFounderGates(now)
	if err != nil {
		log.Printf("founder gates error: %v", err)
		return
	}
	for _, g := range gates {
		won, err := database.MarkFounderGateAlerted(g.ID, now)
		if err != nil || !won {
			continue
		}
		text := fmt.Sprintf("FOUNDER GATE: task '%s' (%s) waits on you — %s", g.Title, g.TaskID, g.Reason)
		meta := fmt.Sprintf(`{"task_id":%q,"alert":"founder_gate","gate_id":%q}`, g.TaskID, g.ID)
		msg, _, err := database.InsertMessageWithDeliveries(g.Project, "relay", ackFounder, "notification", text, text, meta,
			"P1", -1, nil, nil, []string{ackFounder}, "do")
		if err != nil {
			log.Printf("founder gate message error: task %s: %v", g.TaskID, err)
			continue
		}
		notifier.Notify(g.Project, ackFounder, "relay", text, msg.ID)
		log.Printf("FOUNDER GATE: task %s (%s) -> %s", g.TaskID, g.Title, ackFounder)
	}
}

// apiGetFounderGates is the open-gates digest: every open founder gate of the
// project with its age. Path: GET /api/founder-gates?project=
func (r *Relay) apiGetFounderGates(w http.ResponseWriter, req *http.Request) {
	project := req.URL.Query().Get("project")
	if project == "" {
		project = "default"
	}
	gates, err := r.DB.OpenFounderGates(project, r.DB.Now())
	if err != nil {
		http.Error(w, `{"error":"failed to get founder gates"}`, http.StatusInternalServerError)
		return
	}
	if gates == nil {
		gates = []db.FounderGate{}
	}
	writeJSON(w, gates)
}
