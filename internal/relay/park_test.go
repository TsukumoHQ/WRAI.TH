package relay

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"agent-relay/internal/db"
)

// Task d43d844e: parked work never pages anyone; a founder gate alerts once.

// parkTwin is a twin (raw SQL + recording notifier) with handlers on its DB.
func parkTwin(t *testing.T, name string) (*twin, *Handlers) {
	t.Helper()
	w := newTwin(t, name)
	h := NewHandlers(w.d, NewSessionRegistry(server.NewMCPServer("test", "0.0.0")), nil, NewEventBus())
	t.Cleanup(h.Close)
	return w, h
}

func tickAt(w *twin, now time.Time) {
	evaluateObligations(w.d, w.rec, now)
	evaluateFounderGates(w.d, w.rec, now)
}

// ladderMessages are the ACK-chain messages about one task (founder gate
// alerts and park notices excluded).
func ladderMessages(t *testing.T, w *twin, taskID string) []chainMsg {
	t.Helper()
	var out []chainMsg
	for _, m := range chainMessages(t, w) {
		if strings.Contains(m.subject, "title "+taskID) && !strings.HasPrefix(m.subject, "FOUNDER GATE") && !strings.HasPrefix(m.subject, "PARKED:") {
			out = append(out, m)
		}
	}
	return out
}

func gateAlerts(t *testing.T, w *twin, taskID string) []chainMsg {
	t.Helper()
	var out []chainMsg
	for _, m := range chainMessages(t, w) {
		if strings.HasPrefix(m.subject, "FOUNDER GATE") && strings.Contains(m.subject, "title "+taskID) {
			out = append(out, m)
		}
	}
	return out
}

func resultText(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// AC1: park_task silences every ACK rung (notify, escalate, manager, human),
// including a chain already started; unpark / resume restores the ladder with
// a fresh clock.
func TestParkedTaskNeverTriggersAckRungs(t *testing.T) {
	w, h := parkTwin(t, "park-silence")
	now := time.Now().UTC()
	run(t, w,
		seedTask("f1", "pending", ago(5*time.Hour)),
		seedTask("pre", "in-progress", ago(5*time.Hour)),
		seedTask("k1", "pending", ago(5*time.Hour)),
		seedTask("s1", "pending", ago(20*time.Minute)),
		seedTask("r1", "pending", ago(time.Minute)),
		// Assigned, so the pool rule (a same-profile doer busy on pre) never
		// masks the ladder: only the park may silence it.
		setSQL(`UPDATE tasks SET assigned_to = 'dev-1' WHERE id IN ('f1', 'k1', 's1', 'r1')`))

	// s1's chain has started: rung 0 fired before it is parked.
	tickAt(w, now)
	if msgs := ladderMessages(t, w, "s1"); len(msgs) != 1 {
		t.Fatalf("s1 before park: want rung 0, got %+v", msgs)
	}
	before := map[string]int{"f1": len(ladderMessages(t, w, "f1")), "k1": len(ladderMessages(t, w, "k1"))}

	for id, until := range map[string]string{"f1": "founder", "k1": "pre", "s1": "founder", "r1": "founder"} {
		res, _ := h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": id, "reason": "waits on login", "until": until}))
		if res.IsError {
			t.Fatalf("park %s: %s", id, resultText(res))
		}
	}
	for _, at := range []time.Duration{0, time.Hour, 10 * time.Hour, 47 * time.Hour} {
		tickAt(w, now.Add(at))
	}
	if n := len(ladderMessages(t, w, "s1")); n != 1 {
		t.Fatalf("parked s1: no rung after park, got %d messages", n)
	}
	for id, n := range before {
		if got := len(ladderMessages(t, w, id)); got != n {
			t.Fatalf("parked %s: ladder fired %d new messages", id, got-n)
		}
	}

	// A doer cannot park to dodge the ladder.
	res, _ := h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "task_id": "s1", "reason": "x", "until": "founder"}))
	if !res.IsError || !strings.Contains(resultText(res), "FORBIDDEN") {
		t.Fatalf("doer park: want FORBIDDEN, got %s", resultText(res))
	}

	// resume_task unparks r1 (parked before its chain started): the clock
	// restarts at the unpark, so rung 0 fires 20min later, not before.
	if n := len(ladderMessages(t, w, "r1")); n != 0 {
		t.Fatalf("parked r1: want no rung, got %d", n)
	}
	res, _ = h.HandleResumeTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "r1"}))
	if res.IsError || parseJSON(t, res)["released"] != true {
		t.Fatalf("resume parked r1: want released, got %s", resultText(res))
	}
	if h.db.TaskParked("p1", "r1") {
		t.Fatal("r1 still parked after resume_task")
	}
	tickAt(w, time.Now().UTC())
	if n := len(ladderMessages(t, w, "r1")); n != 0 {
		t.Fatalf("unparked r1: clock must restart, got %d messages", n)
	}
	tickAt(w, time.Now().UTC().Add(20*time.Minute))
	if n := len(ladderMessages(t, w, "r1")); n != 1 {
		t.Fatalf("unparked r1 20min later: want one rung, got %d", n)
	}
	if res, _ = h.HandleResumeTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "f1"})); res.IsError {
		t.Fatalf("unpark f1: %s", resultText(res))
	}
	res, _ = h.HandleResumeTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "f1"}))
	if !res.IsError || !strings.Contains(resultText(res), "not blocked") {
		t.Fatalf("resume twice: want refusal, got %s", resultText(res))
	}
}

// AC2: a task parked until another task unparks by itself when that task is
// done (the completing transition settles it, no sweep needed).
func TestParkUntilTaskUnparksWhenDone(t *testing.T) {
	w, h := parkTwin(t, "park-until")
	run(t, w,
		seedTask("pre", "pending", ago(time.Hour)),
		seedTask("k1", "pending", ago(time.Hour)))
	res, _ := h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "k1", "reason": "after pre", "until": "pre"}))
	if res.IsError {
		t.Fatalf("park: %s", resultText(res))
	}
	if !h.db.TaskParked("p1", "k1") {
		t.Fatal("k1 not parked")
	}
	if ready, _, _ := h.db.TaskReadiness("p1", "k1"); ready {
		t.Fatal("k1 parked until pre must not be ready")
	}
	for _, step := range []func() (*mcp.CallToolResult, error){
		func() (*mcp.CallToolResult, error) {
			return h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "task_id": "pre"}))
		},
		func() (*mcp.CallToolResult, error) {
			return h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "task_id": "pre"}))
		},
		func() (*mcp.CallToolResult, error) {
			return h.HandleCompleteTask(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "task_id": "pre", "result": "ok"}))
		},
	} {
		if r, _ := step(); r.IsError {
			t.Fatalf("pre lifecycle: %s", resultText(r))
		}
	}
	if h.db.TaskParked("p1", "k1") {
		t.Fatal("k1 still parked after pre is done")
	}
	if ready, _, _ := h.db.TaskReadiness("p1", "k1"); !ready {
		t.Fatal("k1 must be ready once pre is done")
	}

	// Parking on a task already done is refused, not a silent no-op park.
	res, _ = h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "k1", "reason": "x", "until": "pre"}))
	if !res.IsError || !strings.Contains(resultText(res), "already done") {
		t.Fatalf("park on done task: want refusal, got %s", resultText(res))
	}
}

// AC3: update_task edits blocked_by after dispatch, dispatcher only.
func TestUpdateTaskBlockedByAfterDispatch(t *testing.T) {
	w, h := parkTwin(t, "update-blocked-by")
	run(t, w,
		seedTask("pre", "pending", ago(time.Hour)),
		seedTask("t1", "pending", ago(time.Hour)))

	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{"project": "p1", "as": "dev-1", "task_id": "t1", "blocked_by": []any{"pre"}}))
	if !res.IsError || !strings.Contains(resultText(res), "FORBIDDEN") {
		t.Fatalf("non-dispatcher blocked_by: want FORBIDDEN, got %s", resultText(res))
	}
	res, _ = h.HandleUpdateTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "t1", "blocked_by": []any{"pre"}}))
	if res.IsError {
		t.Fatalf("dispatcher blocked_by add: %s", resultText(res))
	}
	if ready, refs, _ := h.db.TaskReadiness("p1", "t1"); ready || len(refs) != 1 || refs[0].ID != "pre" {
		t.Fatalf("after add: want blocked by pre, got ready=%v refs=%+v", ready, refs)
	}
	if !h.db.TaskHeld("p1", "t1") {
		t.Fatal("after add: pending t1 must be held")
	}
	res, _ = h.HandleUpdateTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "t1", "blocked_by_remove": []any{"pre"}}))
	if res.IsError {
		t.Fatalf("dispatcher blocked_by remove: %s", resultText(res))
	}
	if ready, _, _ := h.db.TaskReadiness("p1", "t1"); !ready {
		t.Fatal("after remove: t1 must be ready")
	}
	if h.db.TaskHeld("p1", "t1") {
		t.Fatal("after remove: t1 hold must be released")
	}
}

// AC4: entering a founder gate (profile human, or parked until founder) alerts
// the founder exactly once per entry, never climbs the ladder, and the GET
// digest lists the open gates with their age.
func TestFounderGateAlertsOnceAndDigest(t *testing.T) {
	w, h := parkTwin(t, "founder-gate")
	now := time.Now().UTC()
	run(t, w,
		seedTask("hu", "pending", ago(5*time.Hour)),
		setSQL(`UPDATE tasks SET profile_slug = 'human' WHERE id = 'hu'`),
		seedTask("fp", "pending", ago(time.Minute)))

	res, _ := h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "fp", "reason": "founder login", "until": "founder"}))
	if res.IsError {
		t.Fatalf("park: %s", resultText(res))
	}
	for _, at := range []time.Duration{0, time.Hour, 10 * time.Hour} {
		tickAt(w, now.Add(at))
	}
	for _, id := range []string{"hu", "fp"} {
		alerts := gateAlerts(t, w, id)
		if len(alerts) != 1 || alerts[0].to != ackFounder {
			t.Fatalf("%s: want exactly one founder alert to %s, got %+v", id, ackFounder, alerts)
		}
		if msgs := ladderMessages(t, w, id); len(msgs) != 0 {
			t.Fatalf("%s: founder gate must not climb the ladder, got %+v", id, msgs)
		}
	}
	if !strings.Contains(gateAlerts(t, w, "fp")[0].subject, "founder login") {
		t.Fatalf("fp alert must carry the park reason: %+v", gateAlerts(t, w, "fp"))
	}

	digest := func() []db.FounderGate {
		rec := httptest.NewRecorder()
		(&Relay{DB: w.d}).apiGetFounderGates(rec, httptest.NewRequest("GET", "/api/founder-gates?project=p1", nil))
		var gates []db.FounderGate
		if err := json.Unmarshal(rec.Body.Bytes(), &gates); err != nil {
			t.Fatalf("digest body %q: %v", rec.Body.String(), err)
		}
		return gates
	}
	gates := digest()
	if len(gates) != 2 {
		t.Fatalf("digest: want 2 open gates, got %+v", gates)
	}
	for _, g := range gates {
		if g.AgeSeconds < 0 || g.EnteredAt == "" || g.Title == "" {
			t.Fatalf("digest gate missing age/title: %+v", g)
		}
	}

	// Leaving the gate closes it; re-entering is a new entry with one alert.
	if res, _ := h.HandleResumeTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "fp"})); res.IsError {
		t.Fatalf("unpark: %s", resultText(res))
	}
	tickAt(w, now)
	if gates = digest(); len(gates) != 1 || gates[0].TaskID != "hu" {
		t.Fatalf("after unpark: want only hu open, got %+v", gates)
	}
	if res, _ := h.HandleParkTask(ctx, call(map[string]any{"project": "p1", "as": "cto", "task_id": "fp", "reason": "again", "until": "founder"})); res.IsError {
		t.Fatalf("re-park: %s", resultText(res))
	}
	tickAt(w, now)
	if n := len(gateAlerts(t, w, "fp")); n != 2 {
		t.Fatalf("re-entry: want a second alert, got %d", n)
	}
}
