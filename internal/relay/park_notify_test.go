package relay

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Task fea65594: one alert per park, until a task's in-review, block from
// accepted, park visible on reads and through the API.

func parkedNotices(t *testing.T, w *twin, taskID string) []chainMsg {
	t.Helper()
	var out []chainMsg
	for _, m := range chainMessages(t, w) {
		if strings.HasPrefix(m.subject, "PARKED:") && strings.Contains(m.subject, "("+taskID+")") {
			out = append(out, m)
		}
	}
	return out
}

type toolFn = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)

func mustOK(t *testing.T, what string, res *mcp.CallToolResult) {
	t.Helper()
	if res == nil || res.IsError {
		t.Fatalf("%s: %s", what, resultText(res))
	}
}

// Exactly one fyi to the dispatcher per park; a re-park with the same reason
// is silent, a new reason or a park after an unpark is a new park.
func TestParkNotifiesDispatcherOncePerPark(t *testing.T) {
	w, h := parkTwin(t, "park-notify")
	now := time.Now().UTC()
	run(t, w,
		seedTask("pre", "pending", ago(time.Hour)),
		seedTask("k1", "pending", ago(time.Hour)))
	park := func(reason, until string) {
		t.Helper()
		res, _ := h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "k1", "reason": reason, "until": until}))
		mustOK(t, "park "+reason, res)
	}

	park("waits on founder OAuth", "founder")
	for _, at := range []time.Duration{0, time.Hour, 10 * time.Hour} {
		tickAt(w, now.Add(at))
	}
	notes := parkedNotices(t, w, "k1")
	if len(notes) != 1 || notes[0].to != "cto" || notes[0].typ != "fyi" || notes[0].action != "none" {
		t.Fatalf("first park: want one no-wake fyi to the dispatcher, got %+v", notes)
	}
	if !strings.Contains(notes[0].subject, "waits on founder OAuth") {
		t.Fatalf("notice must carry the reason: %+v", notes[0])
	}

	park("waits on founder OAuth", "founder")
	park("waits on founder OAuth", "pre")
	if n := len(parkedNotices(t, w, "k1")); n != 1 {
		t.Fatalf("re-park, same reason: want no new notice, got %d", n)
	}
	park("after pre lands", "pre")
	if n := len(parkedNotices(t, w, "k1")); n != 2 {
		t.Fatalf("re-park, new reason: want a second notice, got %d", n)
	}
	mustOK(t, "unpark", func() *mcp.CallToolResult {
		r, _ := h.HandleResumeTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "k1"}))
		return r
	}())
	park("after pre lands", "pre")
	if n := len(parkedNotices(t, w, "k1")); n != 3 {
		t.Fatalf("park after unpark: want a third notice, got %d", n)
	}
}

// until=task:<id>@in-review unparks when that task reaches in-review, and the
// ACK clock restarts at the unpark: no rung until ACKNotifyAge after it.
func TestParkUntilTaskInReviewUnparksAndRestartsClock(t *testing.T) {
	w, h := parkTwin(t, "park-in-review")
	now := time.Now().UTC()
	run(t, w,
		seedTask("pre", "pending", ago(5*time.Hour)),
		seedTask("k1", "pending", ago(5*time.Hour)),
		setSQL(`UPDATE tasks SET assigned_to = 'dev-2' WHERE id = 'k1'`))

	res, _ := h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "k1", "reason": "after pre review", "until": "task:pre@in-review"}))
	mustOK(t, "park", res)
	if got := parseJSON(t, res); got["until"] != "pre" || got["until_status"] != "in-review" {
		t.Fatalf("park result: %+v", got)
	}
	for _, at := range []time.Duration{0, time.Hour, 47 * time.Hour} {
		tickAt(w, now.Add(at))
	}
	if n := len(ladderMessages(t, w, "k1")); n != 0 {
		t.Fatalf("parked k1: want no rung, got %d", n)
	}

	for _, tool := range []toolFn{h.HandleClaimTask, h.HandleStartTask} {
		r, _ := tool(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "task_id": "pre"}))
		mustOK(t, "pre lifecycle", r)
	}
	if !h.db.TaskParked("p1", "k1") {
		t.Fatal("k1 must stay parked while pre is only in progress")
	}
	r, _ := h.HandleReviewTask(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "task_id": "pre", "result": "PR up"}))
	mustOK(t, "pre review", r)
	if h.db.TaskParked("p1", "k1") {
		t.Fatal("k1 still parked after pre reached in-review")
	}

	tickAt(w, time.Now().UTC())
	if n := len(ladderMessages(t, w, "k1")); n != 0 {
		t.Fatalf("unparked k1: clock must restart at the unpark, got %d rungs", n)
	}
	tickAt(w, time.Now().UTC().Add(ACKNotifyAge+time.Minute))
	if n := len(ladderMessages(t, w, "k1")); n != 1 {
		t.Fatalf("unparked k1 past ACKNotifyAge: want one rung, got %d", n)
	}

	// Parking on a task already at the until status is refused.
	res, _ = h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "k1", "reason": "x", "until": "pre@in-review"}))
	if !res.IsError || !strings.Contains(resultText(res), "already in-review") {
		t.Fatalf("park on in-review task: want refusal, got %s", resultText(res))
	}
}

// block_task from accepted succeeds, and resume_task returns the task to
// accepted with its claim and lease back on the doer; a block from
// in-progress still resumes to in-progress.
func TestBlockFromAcceptedResumesToAccepted(t *testing.T) {
	w, h := parkTwin(t, "block-accepted")
	run(t, w,
		seedTask("a1", "pending", ago(time.Hour)),
		seedTask("i1", "pending", ago(time.Hour)))
	do := func(what string, fn toolFn, as, id string) map[string]any {
		t.Helper()
		r, _ := fn(ctx, call(map[string]any{"project": "p1", "as": as, "task_id": id, "reason": "waits on founder OAuth"}))
		mustOK(t, what, r)
		return parseJSON(t, r)
	}
	claim, start, block, resume := h.HandleClaimTask, h.HandleStartTask, h.HandleBlockTask, h.HandleResumeTask

	do("claim a1", claim, "dev-1", "a1")
	if got := do("block a1 from accepted", block, "dev-1", "a1"); got["status"] != "blocked" {
		t.Fatalf("block from accepted: %+v", got)
	}
	got := do("resume a1", resume, "cto", "a1")
	if got["status"] != "accepted" || got["assigned_to"] != "dev-1" || got["lease_holder"] != "dev-1" {
		t.Fatalf("resume a1: want accepted, held by dev-1, got status=%v assigned=%v lease=%v",
			got["status"], got["assigned_to"], got["lease_holder"])
	}
	if bp, _ := got["blocked_periods"].(string); !strings.Contains(bp, `"end"`) {
		t.Fatalf("resume a1: blocked window must close, got %v", got["blocked_periods"])
	}
	do("start a1 after resume", start, "dev-1", "a1")

	do("claim i1", claim, "dev-2", "i1")
	do("start i1", start, "dev-2", "i1")
	do("block i1", block, "dev-2", "i1")
	if got := do("resume i1", resume, "dev-2", "i1"); got["status"] != "in-progress" {
		t.Fatalf("resume from in-progress block: want in-progress, got %v", got["status"])
	}
}

// The park is visible on get_task, list_tasks (json and md) and GET
// /api/tasks/{id}; POST /api/tasks/{id}/park|unpark drive it.
func TestParkVisibleOnReadsAndAPI(t *testing.T) {
	w, h := parkTwin(t, "park-visible")
	run(t, w,
		seedTask("pre", "pending", ago(time.Hour)),
		seedTask("k1", "pending", ago(time.Hour)))
	r := &Relay{DB: w.d, Handlers: h}
	api := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeAPI(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := api("POST", "/api/tasks/k1/park", `{"project":"p1","reason":"founder OAuth","until":"founder"}`)
	if code != 200 || out["parked"] != true {
		t.Fatalf("API park: %d %+v", code, out)
	}
	if n := len(parkedNotices(t, w, "k1")); n != 1 {
		t.Fatalf("API park: want one dispatcher notice, got %d", n)
	}
	wantPark := func(where string, task map[string]any) {
		t.Helper()
		p, _ := task["park"].(map[string]any)
		if p == nil || p["until"] != "founder" || p["reason"] != "founder OAuth" || p["parked_by"] != "user" || p["since"] == "" {
			t.Fatalf("%s: want park {founder, founder OAuth, user}, got %+v", where, task["park"])
		}
	}
	gt, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": "k1"}))
	wantPark("get_task", parseJSON(t, gt))
	lt, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	for _, raw := range parseJSON(t, lt)["tasks"].([]any) {
		task := raw.(map[string]any)
		if task["id"] == "k1" {
			wantPark("list_tasks", task)
		} else if task["park"] != nil {
			t.Fatalf("list_tasks: unparked %v carries a park", task["id"])
		}
	}
	md, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "p1"}))
	if !strings.Contains(resultText(md), "pending (parked until founder)") {
		t.Fatalf("list_tasks md: want parked status, got %s", resultText(md))
	}
	_, out = api("GET", "/api/tasks/k1?project=p1", "")
	wantPark("GET /api/tasks/k1", out)

	if code, out = api("POST", "/api/tasks/k1/unpark", `{"project":"p1"}`); code != 200 || out["parked"] != false {
		t.Fatalf("API unpark: %d %+v", code, out)
	}
	if _, out = api("GET", "/api/tasks/k1?project=p1", ""); out["park"] != nil {
		t.Fatalf("after unpark: park must be gone, got %+v", out["park"])
	}
	if code, out = api("POST", "/api/tasks/k1/unpark", `{"project":"p1"}`); code != 400 || !strings.Contains(out["detail"].(string), "TASK_NOT_PARKED") {
		t.Fatalf("unpark twice: want 400 TASK_NOT_PARKED, got %d %+v", code, out)
	}
	if code, _ = api("POST", "/api/tasks/k1/park", `{"project":"p1","reason":"x"}`); code != 400 {
		t.Fatalf("API park without until: want 400, got %d", code)
	}
}
