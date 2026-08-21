package relay

import "testing"

// TestHandleDispatchTask_TraceID_RejectsInvalidFormat: a malformed trace_id is
// refused explicitly, never silently coerced or dropped — the task must NOT
// be created.
func TestHandleDispatchTask_TraceID_RejectsInvalidFormat(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "bad trace", "trace_id": "not-hex",
	}))
	expectError(t, res)

	listRes, _ := h.HandleListTasks(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	if got := parseJSON(t, listRes)["count"].(float64); got != 0 {
		t.Fatalf("refused dispatch must not create a task, got %v tasks", got)
	}
}

// TestHandleDispatchTask_TraceID_ExplicitValidWorks: a well-formed
// caller-supplied trace_id is accepted and surfaced on the dispatch response.
func TestHandleDispatchTask_TraceID_ExplicitValidWorks(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	want := "cccc0000cccc0000cccc0000cccc0000"[:32]
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "good trace", "trace_id": want,
	}))
	if res.IsError {
		t.Fatalf("valid trace_id must dispatch: %s", expectError(t, res))
	}
	task := parseJSON(t, res)["task"].(map[string]any)
	if task["trace_id"] != want {
		t.Errorf("trace_id = %v, want %q", task["trace_id"], want)
	}
}

// TestEmitTaskEvent_DispatchCarriesTraceID: the task.dispatched event's
// Semantic payload carries the freshly-minted trace_id.
func TestEmitTaskEvent_DispatchCarriesTraceID(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))

	ch := h.events.Subscribe()
	defer h.events.Unsubscribe(ch)

	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "traced dispatch",
	}))
	if res.IsError {
		t.Fatalf("dispatch failed: %s", expectError(t, res))
	}
	task := parseJSON(t, res)["task"].(map[string]any)
	wantTrace, _ := task["trace_id"].(string)
	if wantTrace == "" {
		t.Fatalf("dispatch response missing trace_id: %+v", task)
	}

	for i := 0; i < 20; i++ {
		select {
		case ev := <-ch:
			if ev.Type != "task.dispatched" {
				continue
			}
			got, _ := ev.Semantic["trace_id"].(string)
			if got != wantTrace {
				t.Fatalf("task.dispatched Semantic trace_id = %q, want %q", got, wantTrace)
			}
			return
		default:
			t.Fatal("no more buffered events, task.dispatched not seen")
		}
	}
	t.Fatal("task.dispatched event not observed within 20 events")
}
