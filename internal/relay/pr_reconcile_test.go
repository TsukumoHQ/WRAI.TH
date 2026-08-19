package relay

import (
	"testing"

	"agent-relay/internal/db"
)

// The reconcile candidate resource lists ONLY tasks with a linked open PR whose
// task is non-terminal — the set an external poller GETs to catch a missed
// webhook. Merged/closed PRs and terminal tasks are excluded.
func TestPRReconcileCandidates(t *testing.T) {
	r := testRelay(t)

	// (1) open PR, non-terminal task → candidate.
	open, _ := r.DB.DispatchTask("p1", "dev", "boss", "open pr", "", "P2", nil, nil, db.TypedTicket{}, false)
	_, _ = r.DB.SetTaskPR(open.ID, "p1", ptr("u1"), intp(1), ptr("open"), ptr("o/r"))

	// (2) merged PR → excluded (terminal PR state).
	merged, _ := r.DB.DispatchTask("p1", "dev", "boss", "merged pr", "", "P2", nil, nil, db.TypedTicket{}, false)
	_, _ = r.DB.SetTaskPR(merged.ID, "p1", ptr("u2"), intp(2), ptr("merged"), ptr("o/r"))

	// (3) open PR but task done → excluded (terminal task).
	doneTask, _ := r.DB.DispatchTask("p1", "dev", "boss", "done task", "", "P2", nil, nil, db.TypedTicket{}, false)
	_, _ = r.DB.SetTaskPR(doneTask.ID, "p1", ptr("u3"), intp(3), ptr("open"), ptr("o/r"))
	if _, _, err := r.DB.ForcePRTransition("p1", doneTask.ID, "done", nil); err != nil {
		t.Fatalf("force done: %v", err)
	}

	// (4) no PR linked → excluded.
	_, _ = r.DB.DispatchTask("p1", "dev", "boss", "no pr", "", "P2", nil, nil, db.TypedTicket{}, false)

	cands, err := r.DB.ListPRReconcileCandidates("p1", 200)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != open.ID {
		ids := make([]string, len(cands))
		for i, c := range cands {
			ids[i] = c.ID
		}
		t.Fatalf("expected only the open-PR non-terminal task, got %v", ids)
	}

	// The resource surfaces the same set, scoped + compact.
	rc, rerr := r.Handlers.resourcePRReconcile(ctxProject("p1"), readReq(resourcePRReconcile))
	body := readResource(t, rc, rerr, resourcePRReconcile)
	rows, _ := body["candidates"].([]any)
	if len(rows) != 1 {
		t.Fatalf("resource should list 1 candidate, got %v", body)
	}
	row, _ := rows[0].(map[string]any)
	if row["pr_number"] != float64(1) || row["pr_repo"] != "o/r" {
		t.Fatalf("candidate row missing pr fields: %v", row)
	}
}

func TestPRReconcileUnscopedHint(t *testing.T) {
	r := testRelay(t)
	rc, rerr := r.Handlers.resourcePRReconcile(ctxProject(""), readReq(resourcePRReconcile))
	body := readResource(t, rc, rerr, resourcePRReconcile)
	if body["project"] != "" || body["note"] == nil {
		t.Fatalf("unscoped reconcile read should return a hint: %v", body)
	}
}

func intp(i int) *int { return &i }
