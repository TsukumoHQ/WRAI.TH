package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// park P1 11300e39 — demote_task moves pending native tasks back to backlog,
// single or batch (design docs/design/park.md §3, ruling wraith-park-ruling).

// demoteFleet: lead dispatches to dev; outsider is an unrelated plain agent.
func demoteFleet(t *testing.T) *Handlers {
	h := testHandlers(t)
	for _, a := range []map[string]any{
		{"project": "p1", "name": "lead"},
		{"project": "p1", "name": "dev", "reports_to": "lead"},
		{"project": "p1", "name": "outsider"},
	} {
		if res, _ := h.HandleRegisterAgent(ctx, call(a)); res.IsError {
			t.Fatalf("register %v: %s", a["name"], expectError(t, res))
		}
	}
	return h
}

func demoteDispatch(t *testing.T, h *Handlers, title string) string {
	t.Helper()
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "lead", "profile": "dev", "title": title}))
	if res.IsError {
		t.Fatalf("dispatch %s: %s", title, expectError(t, res))
	}
	return parseJSON(t, res)["task"].(map[string]any)["id"].(string)
}

func demote(h *Handlers, args map[string]any) *mcp.CallToolResult {
	res, _ := h.guardIdentity("demote_task", h.HandleDemoteTask)(ctx, call(args))
	return res
}

func taskStatus(t *testing.T, h *Handlers, id string) string {
	t.Helper()
	task, err := h.db.GetTask(id, "p1")
	if err != nil || task == nil {
		t.Fatalf("get task %s: %v", id, err)
	}
	return task.Status
}

// AC1 — demote_task(pending, reason) -> backlog, with an audit row carrying
// the reason and the actor.
func TestDemotePendingToBacklogAudited(t *testing.T) {
	h := demoteFleet(t)
	id := demoteDispatch(t, h, "groom me")

	res := demote(h, map[string]any{"project": "p1", "as": "lead", "task_id": id, "reason": "not this sprint"})
	if res.IsError {
		t.Fatalf("demote: %s", expectError(t, res))
	}
	if got := taskStatus(t, h, id); got != "backlog" {
		t.Fatalf("status after demote: want backlog, got %s", got)
	}
	rows, _ := h.db.ListAudit("p1", id, 20)
	var found bool
	for _, r := range rows {
		if r.Action == "task.demoted" && r.Actor == "lead" && r.Reason == "not this sprint" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a task.demoted audit row (actor lead, reason), got %+v", rows)
	}
	if res := demote(h, map[string]any{"project": "p1", "as": "lead", "task_id": id}); !res.IsError {
		t.Fatal("reason is required")
	}
}

// AC2 — a caller who is neither the dispatcher, an executive nor in the
// doer's lead chain is refused, and the task stays pending.
func TestDemoteRefusesUnauthorizedCaller(t *testing.T) {
	h := demoteFleet(t)
	id := demoteDispatch(t, h, "not yours")
	for _, who := range []string{"outsider", "dev"} {
		res := demote(h, map[string]any{"project": "p1", "as": who, "task_id": id, "reason": "x"})
		if body := decodeToolError(t, res); body["code"] != CodeForbidden {
			t.Fatalf("%s demoting: want %s, got %v", who, CodeForbidden, body)
		}
	}
	if got := taskStatus(t, h, id); got != "pending" {
		t.Fatalf("a refused demote must not move the task, got %s", got)
	}
}

// AC3 — a Linear mirror is refused with a hint naming park_task.
func TestDemoteRefusesLinearMirror(t *testing.T) {
	h := demoteFleet(t)
	if res, _ := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "boss", "is_executive": true})); res.IsError {
		t.Fatalf("register boss: %s", expectError(t, res))
	}
	if err := h.db.UpsertLinearTask(db.LinearTaskSeed{ID: "lt-1", Project: "p1", Title: "mirror", Status: "pending"}); err != nil {
		t.Fatalf("upsert linear task: %v", err)
	}
	res := demote(h, map[string]any{"project": "p1", "as": "boss", "task_id": "lt-1", "reason": "x"})
	body := decodeToolError(t, res)
	if !strings.Contains(body["message"].(string), "park_task") {
		t.Fatalf("Linear mirror refusal must name park_task, got %v", body)
	}
	if got := taskStatus(t, h, "lt-1"); got != "pending" {
		t.Fatalf("a refused demote must not move the mirror, got %s", got)
	}
}

// AC4 — a batch of 3 ids (1 not pending) reports each result; one refusal
// does not abort the rest.
func TestDemoteBatchReportsEachID(t *testing.T) {
	h := demoteFleet(t)
	a := demoteDispatch(t, h, "a")
	b := demoteDispatch(t, h, "b")
	c := demoteDispatch(t, h, "c")
	if res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "dev", "task_id": b})); res.IsError {
		t.Fatalf("claim b: %s", expectError(t, res))
	}

	res := demote(h, map[string]any{"project": "p1", "as": "lead", "task_ids": []any{a, b, c}, "reason": "regroom"})
	if res.IsError {
		t.Fatalf("batch demote: %s", expectError(t, res))
	}
	var out struct {
		Results []struct {
			TaskID string `json:"task_id"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
		Demoted int `json:"demoted"`
		Total   int `json:"total"`
	}
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out)
	if out.Total != 3 || out.Demoted != 2 || len(out.Results) != 3 {
		t.Fatalf("batch summary: %+v", out)
	}
	for _, r := range out.Results {
		switch r.TaskID {
		case a, c:
			if r.Status != "demoted" || r.Error != "" {
				t.Fatalf("%s: want demoted, got %+v", r.TaskID, r)
			}
		case b:
			if r.Status != "refused" || !strings.Contains(r.Error, "accepted") {
				t.Fatalf("claimed task: want refused naming its status, got %+v", r)
			}
		default:
			t.Fatalf("unexpected id in results: %+v", r)
		}
	}
	if taskStatus(t, h, a) != "backlog" || taskStatus(t, h, c) != "backlog" || taskStatus(t, h, b) != "accepted" {
		t.Fatalf("statuses: a=%s b=%s c=%s", taskStatus(t, h, a), taskStatus(t, h, b), taskStatus(t, h, c))
	}
}
