package relay

import (
	"errors"
	"strings"
	"testing"

	"agent-relay/internal/db"
)

// The loophole this task closes: dispatchCore is the shared create path behind
// cron scheduled dispatch AND the inbound-signal webhook (which self-dispatches a
// followup with a BARE db.TypedTicket{}). Those paths skipped the old per-handler
// guard, so a bare ticket slipped through on an enforced project and every scribe
// doc built from it rendered empty. Now the single guard in db.DispatchTask fires
// for dispatchCore too: a bare self-dispatch on niwa is refused as a typed error,
// and a complete one still lands.
func TestDispatchCore_TypedTicket_RefusesBareSelfDispatch(t *testing.T) {
	h := testHandlers(t)

	_, _, err := h.dispatchCore("niwa", "backend-lead", "dev", "review followup", "", "P2", nil, nil, db.TypedTicket{}, false)
	var tte *db.TypedTicketError
	if !errors.As(err, &tte) {
		t.Fatalf("bare self-dispatch via dispatchCore on an enforced project must refuse, got %v", err)
	}
	if strings.Join(tte.Missing, ",") != "goal,acceptance_criteria,dod" {
		t.Fatalf("missing = %v, want all three", tte.Missing)
	}

	task, _, err := h.dispatchCore("niwa", "backend-lead", "dev", "typed followup", "", "P2", nil, nil,
		db.TypedTicket{Goal: "g", AcceptanceCriteria: `["a"]`, Dod: "d"}, false)
	if err != nil {
		t.Fatalf("complete self-dispatch must land: %v", err)
	}
	if task == nil || task.Status != "pending" {
		t.Fatalf("expected pending task, got %+v", task)
	}
}

// The guard is hoisted ahead of dispatchCore's board/profile auto-create: a
// refused bare dispatch on an enforced project with no board must leave NO stray
// empty "Backlog" board (review-b092a6be finding). Enforcement fails fast, before
// any side-effect.
func TestDispatchCore_TypedTicket_RefusalLeavesNoStrayBoard(t *testing.T) {
	h := testHandlers(t)

	if boards, _ := h.db.ListBoards("niwa"); len(boards) != 0 {
		t.Fatalf("precondition: niwa should start with no boards, got %d", len(boards))
	}
	// boardID nil → dispatchCore would auto-create a Backlog board if it reached
	// that far. A bare ticket must be refused before that.
	if _, _, err := h.dispatchCore("niwa", "cto", "dev", "bare", "", "P2", nil, nil, db.TypedTicket{}, false); err == nil {
		t.Fatal("bare dispatch must be refused")
	}
	if boards, _ := h.db.ListBoards("niwa"); len(boards) != 0 {
		t.Fatalf("refused dispatch left a stray board: %d board(s)", len(boards))
	}
}

// On a typed-ticket project (niwa is seeded on) an incomplete dispatch is
// refused, and the error names exactly the missing field(s).
func TestDispatchTask_TypedTicket_RefusesMissingFields(t *testing.T) {
	h := testHandlers(t)

	cases := []struct {
		name string
		args map[string]any
		want string // exact bracketed missing-list, in dispatch order
	}{
		{
			name: "all missing",
			args: map[string]any{"project": "niwa", "as": "cto", "profile": "dev", "title": "t"},
			want: "goal, acceptance_criteria, dod",
		},
		{
			name: "goal missing",
			args: map[string]any{"project": "niwa", "as": "cto", "profile": "dev", "title": "t", "acceptance_criteria": `["x"]`, "dod": "green"},
			want: "goal",
		},
		{
			name: "acceptance_criteria missing",
			args: map[string]any{"project": "niwa", "as": "cto", "profile": "dev", "title": "t", "goal": "g", "dod": "green"},
			want: "acceptance_criteria",
		},
		{
			name: "dod missing",
			args: map[string]any{"project": "niwa", "as": "cto", "profile": "dev", "title": "t", "goal": "g", "acceptance_criteria": `["x"]`},
			want: "dod",
		},
		{
			name: "acceptance_criteria empty array is not a list",
			args: map[string]any{"project": "niwa", "as": "cto", "profile": "dev", "title": "t", "goal": "g", "acceptance_criteria": `[]`, "dod": "green"},
			want: "acceptance_criteria",
		},
		{
			name: "acceptance_criteria blank items is not a list",
			args: map[string]any{"project": "niwa", "as": "cto", "profile": "dev", "title": "t", "goal": "g", "acceptance_criteria": `["  ",""]`, "dod": "green"},
			want: "acceptance_criteria",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := h.HandleDispatchTask(ctx, call(tc.args))
			msg := expectError(t, res)
			if got := bracketed(t, msg); got != tc.want {
				t.Errorf("missing list = [%s], want [%s]; full msg: %s", got, tc.want, msg)
			}
		})
	}
}

// bracketed extracts the "[...]" missing-field list out of a ticket refusal so
// the assertion reads the actual list, not the guidance sentence that always
// names every field key.
func bracketed(t *testing.T, msg string) string {
	t.Helper()
	open := strings.Index(msg, "[")
	closeI := strings.Index(msg, "]")
	if open < 0 || closeI < 0 || closeI < open {
		t.Fatalf("refusal has no bracketed missing list: %s", msg)
	}
	return msg[open+1 : closeI]
}

// A complete ticket dispatches and the three fields read back through get_task.
func TestDispatchTask_TypedTicket_CompletePersistsAndReadsBack(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "niwa", "as": "cto", "profile": "dev", "title": "typed",
		"goal":                "determinise task birth",
		"acceptance_criteria": `["refuses without goal","reads back"]`,
		"dod":                 "go test ./... green",
	}))
	task := parseJSON(t, res)["task"].(map[string]any)
	if task["goal"] != "determinise task birth" {
		t.Fatalf("goal not persisted: %v", task["goal"])
	}
	if task["dod"] != "go test ./... green" {
		t.Fatalf("dod not persisted: %v", task["dod"])
	}
	taskID := task["id"].(string)

	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "niwa", "task_id": taskID}))
	got := parseJSON(t, getRes)
	if got["goal"] != "determinise task birth" {
		t.Errorf("get_task lost goal: %v", got["goal"])
	}
	if got["acceptance_criteria"] != `["refuses without goal","reads back"]` {
		t.Errorf("get_task lost acceptance_criteria: %v", got["acceptance_criteria"])
	}
	if got["dod"] != "go test ./... green" {
		t.Errorf("get_task lost dod: %v", got["dod"])
	}
}

// Retrocompat: a project without the flag dispatches free-form, no ticket
// needed — the relay serves many such projects and must not break them.
func TestDispatchTask_FreeFormProject_NoTicketNeeded(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "cto", "profile": "dev", "title": "free form",
	}))
	task := parseJSON(t, res)["task"].(map[string]any)
	if task["status"] != "pending" {
		t.Fatalf("free-form dispatch should succeed, got %+v", task)
	}
	if task["acceptance_criteria"] != "[]" {
		t.Errorf("absent acceptance_criteria should default to '[]', got %v", task["acceptance_criteria"])
	}
}

// batch_dispatch_tasks follows the same per-project rule, per item: a complete
// item dispatches, an incomplete one lands in errors while the rest proceed.
func TestBatchDispatch_TypedTicket_PerItem(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleBatchDispatchTasks(ctx, call(map[string]any{
		"project": "niwa", "as": "cto",
		"tasks": `[
			{"profile":"dev","title":"complete","goal":"g","acceptance_criteria":["a","b"],"dod":"green"},
			{"profile":"dev","title":"missing dod","goal":"g","acceptance_criteria":["a"]}
		]`,
	}))
	body := parseJSON(t, res)

	dispatched, _ := body["dispatched"].([]any)
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched, got %d: %+v", len(dispatched), body["dispatched"])
	}
	errs, _ := body["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %+v", len(errs), body["errors"])
	}
	if e := errs[0].(string); !strings.Contains(e, "missing dod") || !strings.Contains(e, "dod") {
		t.Errorf("batch error should name the incomplete task and the missing field; got: %s", e)
	}
}

// Same batch on a free-form project dispatches every item, ticket or not.
func TestBatchDispatch_FreeFormProject(t *testing.T) {
	h := testHandlers(t)

	res, _ := h.HandleBatchDispatchTasks(ctx, call(map[string]any{
		"project": "p1", "as": "cto",
		"tasks": `[{"profile":"dev","title":"one"},{"profile":"dev","title":"two"}]`,
	}))
	body := parseJSON(t, res)
	dispatched, _ := body["dispatched"].([]any)
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched on free-form project, got %d: %+v", len(dispatched), body["dispatched"])
	}
}
