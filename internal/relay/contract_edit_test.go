package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

// 04ac0ae3 — a dead dispatcher no longer freezes its tickets' contract: the
// dispatcher's lead chain (once the dispatcher is inactive) and executives may
// edit goal/acceptance_criteria/dod/verify_cmd, audited as task.contract_edited;
// the doer and anyone below it never may, even an executive doer.

// contractFleet mirrors the field case: niwa-cto (dispatcher) reports to
// niwa-cto-2; dev does the work and intern reports to dev; boss is an
// executive; xdev is an executive who also takes work.
func contractFleet(t *testing.T) (*Handlers, string) {
	h := testHandlers(t)
	for _, a := range []map[string]any{
		{"project": "p1", "name": "niwa-cto-2"},
		{"project": "p1", "name": "niwa-cto", "reports_to": "niwa-cto-2"},
		{"project": "p1", "name": "dev"},
		{"project": "p1", "name": "intern", "reports_to": "dev"},
		{"project": "p1", "name": "boss", "is_executive": true},
		{"project": "p1", "name": "xdev", "is_executive": true},
	} {
		if res, _ := h.HandleRegisterAgent(ctx, call(a)); res.IsError {
			t.Fatalf("register %v: %s", a["name"], expectError(t, res))
		}
	}
	return h, contractDispatch(t, h, "dev")
}

func contractDispatch(t *testing.T, h *Handlers, profile string) string {
	t.Helper()
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "niwa-cto", "profile": profile, "title": "ship it",
		"goal": "g0", "acceptance_criteria": `["test: a"]`, "dod": "merged",
	}))
	if res.IsError {
		t.Fatalf("dispatch: %s", expectError(t, res))
	}
	id := parseJSON(t, res)["task"].(map[string]any)["id"].(string)
	if res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": profile, "task_id": id})); res.IsError {
		t.Fatalf("claim by %s: %s", profile, expectError(t, res))
	}
	return id
}

func editGoal(t *testing.T, h *Handlers, as, id, goal string) (bool, string) {
	t.Helper()
	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{"project": "p1", "as": as, "task_id": id, "goal": goal}))
	if res.IsError {
		return false, expectError(t, res)
	}
	return true, ""
}

func contractEdits(t *testing.T, h *Handlers, id string) []map[string]any {
	t.Helper()
	rows, _ := h.db.ListAudit("p1", id, 50)
	var out []map[string]any
	for _, r := range rows {
		if r.Action != "task.contract_edited" {
			continue
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(r.Details), &d)
		out = append(out, d)
	}
	return out
}

// AC1 — the dispatcher edits its contract, as today.
func TestContractEditByDispatcher(t *testing.T) {
	h, id := contractFleet(t)
	if ok, msg := editGoal(t, h, "niwa-cto", id, "g1"); !ok {
		t.Fatalf("dispatcher edit refused: %s", msg)
	}
}

// AC2 — once the dispatcher is inactive, an agent above it in the reports_to
// chain may edit; an executive always may. Each edit is audited with by,
// fields and before/after hashes.
func TestContractEditByChainAndExecutiveAudited(t *testing.T) {
	h, id := contractFleet(t)
	if ok, _ := editGoal(t, h, "niwa-cto-2", id, "g1"); ok {
		t.Fatal("the dispatcher's lead may not edit while the dispatcher is alive")
	}
	if err := h.db.DeactivateAgent("p1", "niwa-cto"); err != nil {
		t.Fatalf("deactivate dispatcher: %v", err)
	}
	if ok, msg := editGoal(t, h, "niwa-cto-2", id, "g1"); !ok {
		t.Fatalf("the dead dispatcher's lead must be able to edit: %s", msg)
	}
	if ok, msg := editGoal(t, h, "boss", id, "g2"); !ok {
		t.Fatalf("an executive must be able to edit: %s", msg)
	}

	edits := contractEdits(t, h, id)
	if len(edits) != 2 {
		t.Fatalf("want 2 task.contract_edited rows, got %v", edits)
	}
	byWho := map[string]map[string]any{}
	for _, e := range edits {
		byWho[e["by"].(string)] = e
	}
	for _, who := range []string{"niwa-cto-2", "boss"} {
		e := byWho[who]
		if e == nil {
			t.Fatalf("no contract_edited row by %s: %v", who, edits)
		}
		fields, _ := e["fields"].([]any)
		if len(fields) != 1 || fields[0] != "goal" {
			t.Fatalf("%s: fields want [goal], got %v", who, e["fields"])
		}
		if e["before_hash"] == "" || e["before_hash"] == e["after_hash"] {
			t.Fatalf("%s: before/after hashes must differ, got %v", who, e)
		}
	}
}

// AC3 — the doer never edits its own contract, even as an executive; nor does
// an agent below the doer.
func TestContractEditRefusesDoerAndBelow(t *testing.T) {
	h, id := contractFleet(t)
	for _, who := range []string{"dev", "intern"} {
		if ok, _ := editGoal(t, h, who, id, "weaker"); ok {
			t.Fatalf("%s must not edit the contract of a task dev holds", who)
		}
	}
	xid := contractDispatch(t, h, "xdev")
	ok, msg := editGoal(t, h, "xdev", xid, "weaker")
	if ok || !strings.Contains(msg, "xdev") {
		t.Fatalf("an executive doer must be refused on its own task, got ok=%v %q", ok, msg)
	}
	if edits := contractEdits(t, h, xid); len(edits) != 0 {
		t.Fatalf("a refused edit must not be audited as an edit: %v", edits)
	}
}

// AC4 — a contract edit while the task is in review posts a progress note so
// the gate's AC refresh picks it up.
func TestContractEditInReviewPostsProgressNote(t *testing.T) {
	h, id := contractFleet(t)
	if res, _ := h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "dev", "task_id": id})); res.IsError {
		t.Fatalf("start: %s", expectError(t, res))
	}
	if res, _ := h.HandleReviewTask(ctx, call(map[string]any{"project": "p1", "as": "dev", "task_id": id})); res.IsError {
		t.Fatalf("review: %s", expectError(t, res))
	}
	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{"project": "p1", "as": "boss", "task_id": id, "acceptance_criteria": `["test: a","receipt: b"]`}))
	if res.IsError {
		t.Fatalf("executive AC edit in review: %s", expectError(t, res))
	}
	notes, err := h.db.GetProgressNotes(id, "p1")
	if err != nil {
		t.Fatalf("progress notes: %v", err)
	}
	var found bool
	for _, n := range notes {
		if n.Agent == "boss" && strings.Contains(n.Note, "contract edited") && strings.Contains(n.Note, "acceptance_criteria") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an in-review contract edit must post a progress note naming the fields, got %+v", notes)
	}
}
