package relay

import (
	"encoding/json"
	"net/http"
	"testing"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// Task e2273dc3 — WIP limit through the served surfaces.

func dispatchFor(t *testing.T, h *Handlers, dispatcher, title string) string {
	t.Helper()
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": dispatcher, "profile": "dev", "title": title}))
	if res.IsError {
		t.Fatalf("dispatch %s: %s", title, expectError(t, res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out)
	task, _ := out["task"].(map[string]any)
	id, _ := task["id"].(string)
	return id
}

// claim_task (by id and next) surfaces WIP_LIMIT as a typed, non-retryable
// refusal; force from the task's dispatcher passes.
func TestClaimToolWIPLimit(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "lead", nil)
	registerActive(t, h, "p1", "bot-a", map[string]any{"profile_slug": "dev"})
	t1 := dispatchFor(t, h, "lead", "one")
	t2 := dispatchFor(t, h, "lead", "two")
	if res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": t1})); res.IsError {
		t.Fatalf("first claim: %s", expectError(t, res))
	}
	for _, args := range []map[string]any{
		{"project": "p1", "as": "bot-a", "task_id": t2},
		{"project": "p1", "as": "bot-a", "next": true},
	} {
		res, _ := h.HandleClaimTask(ctx, call(args))
		body := decodeToolError(t, res)
		if body["code"] != db.CodeWIPLimit || body["isRetryable"] != false {
			t.Fatalf("claim %v: want non-retryable %s, got %v", args, db.CodeWIPLimit, body)
		}
	}
	// bot-a's own force is not an override.
	res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "task_id": t2, "force": true}))
	if body := decodeToolError(t, res); body["code"] != db.CodeWIPLimit {
		t.Fatalf("doer force: want %s, got %v", db.CodeWIPLimit, body)
	}
	// The dispatcher, already holding one, forces a second claim.
	t3 := dispatchFor(t, h, "lead", "three")
	if res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "lead", "task_id": t3})); res.IsError {
		t.Fatalf("lead first claim: %s", expectError(t, res))
	}
	if res, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "lead", "task_id": t2, "force": true})); res.IsError {
		t.Fatalf("dispatcher force claim: %s", expectError(t, res))
	}
}

// GET /api/wip lists agents over the limit; PATCH /api/projects/{p} sets it.
func TestWIPReportAndSettingEndpoints(t *testing.T) {
	r := testRelay(t)
	if res, _ := r.Handlers.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"})); res.IsError {
		t.Fatalf("register: %s", expectError(t, res))
	}
	if w := doAPI(r, http.MethodPatch, "/projects/p1", `{"wip_limit":0}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH wip_limit 0: %d %s", w.Code, w.Body.String())
	}
	for _, title := range []string{"one", "two"} {
		task, err := r.DB.DispatchTask("p1", "", "lead", title, "", "P1", nil, nil, db.TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if _, err := r.DB.ClaimTask(task.ID, "bot-a", "p1"); err != nil {
			t.Fatalf("claim %s: %v", title, err)
		}
	}
	if w := doAPI(r, http.MethodPatch, "/projects/p1", `{"wip_limit":-1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH negative: want 400, got %d", w.Code)
	}
	if w := doAPI(r, http.MethodPatch, "/projects/p1", `{"wip_limit":1}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH wip_limit 1: %d %s", w.Code, w.Body.String())
	}
	w := doAPI(r, http.MethodGet, "/wip?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/wip: %d %s", w.Code, w.Body.String())
	}
	var rep struct {
		Limit int               `json:"wip_limit"`
		Over  []db.WIPOverAgent `json:"over_limit"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Limit != 1 || len(rep.Over) != 1 || rep.Over[0].Agent != "bot-a" || len(rep.Over[0].Held) != 2 {
		t.Fatalf("report: want bot-a holding 2 over limit 1, got %+v", rep)
	}
}

// The console's forced move to accepted (REST transition force=true, actor
// "user") passes the WIP limit and is audited like any force.
func TestRESTForcedClaimPassesWIP(t *testing.T) {
	r := testRelay(t)
	if res, _ := r.Handlers.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"})); res.IsError {
		t.Fatalf("register: %s", expectError(t, res))
	}
	var ids []string
	for _, title := range []string{"one", "two"} {
		task, err := r.DB.DispatchTask("p1", "", "lead", title, "", "P1", nil, nil, db.TypedTicket{}, false, nil)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		ids = append(ids, task.ID)
	}
	for _, id := range ids {
		if w := doAPI(r, http.MethodPost, "/tasks/"+id+"/transition", `{"project":"p1","status":"accepted","force":true}`); w.Code != http.StatusOK {
			t.Fatalf("forced accept %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	w := doAPI(r, http.MethodPost, "/tasks/"+ids[1]+"/transition", `{"project":"p1","status":"pending"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reset: %d", w.Code)
	}
	w = doAPI(r, http.MethodPost, "/tasks/"+ids[1]+"/transition", `{"project":"p1","status":"accepted"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unforced accept over the limit: want 400, got %d %s", w.Code, w.Body.String())
	}
}
