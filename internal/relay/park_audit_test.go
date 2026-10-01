package relay

import (
	"testing"

	"agent-relay/internal/db"
)

// Park P3 (ruling wraith-park-ruling Q1): the lead a Linear mirror routes to,
// and its reports_to chain, may park the pending mirror without an executive;
// every park and unpark is audited with actor and reason.

func p3Fixture(t *testing.T) (*Handlers, string) {
	t.Helper()
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "growth-cto"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "content-lead", "reports_to": "growth-cto"}))
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "peer"}))
	id, _, err := h.db.UpsertLinearMirror(db.LinearMirrorSeed{Project: "p1", LinearIssueID: "iss-p3", Title: "mirror", ProfileSlug: "content-lead"})
	if err != nil {
		t.Fatal(err)
	}
	return h, id
}

func p3Park(h *Handlers, id, as, reason string) (bool, string) {
	res, _ := h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": as, "task_id": id, "reason": reason, "until": "founder"}))
	if res.IsError {
		return false, resultText(res)
	}
	return true, ""
}

func TestParkMirror_RoutedLeadAndChainMayPark(t *testing.T) {
	for _, who := range []string{"content-lead", "growth-cto"} {
		t.Run(who, func(t *testing.T) {
			h, id := p3Fixture(t)
			if ok, msg := p3Park(h, id, who, "waiting"); !ok {
				t.Fatalf("%s park of its lane's pending mirror refused: %s", who, msg)
			}
			if !h.db.TaskParked("p1", id) {
				t.Errorf("%s park: task not parked", who)
			}
		})
	}
}

func TestParkMirror_PeerOutsideChainRefused(t *testing.T) {
	h, id := p3Fixture(t)
	if ok, _ := p3Park(h, id, "peer", "waiting"); ok {
		t.Fatal("peer outside the lane's chain parked the mirror, want refused")
	}
	if h.db.TaskParked("p1", id) {
		t.Error("refused park left the task parked")
	}
}

func TestParkAndUnpark_AreAudited(t *testing.T) {
	h, id := p3Fixture(t)
	if ok, msg := p3Park(h, id, "content-lead", "waiting on the founder"); !ok {
		t.Fatal(msg)
	}
	res, _ := h.HandleResumeTask(ctx, call(map[string]any{"project": "p1", "as": "growth-cto", "task_id": id}))
	if res.IsError {
		t.Fatalf("unpark: %s", expectError(t, res))
	}
	entries, err := h.db.ListAudit("p1", id, 20)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{}
	for _, e := range entries {
		got[e.Action] = [2]string{e.Actor, e.Reason}
	}
	if a := got["task.parked"]; a[0] != "content-lead" || a[1] != "waiting on the founder" {
		t.Errorf("task.parked audit = %v, want actor content-lead reason 'waiting on the founder' (all: %v)", a, got)
	}
	if a := got["task.unparked"]; a[0] != "growth-cto" || a[1] != "waiting on the founder" {
		t.Errorf("task.unparked audit = %v, want actor growth-cto reason of the lifted park (all: %v)", a, got)
	}
}
