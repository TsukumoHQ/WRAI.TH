package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

// W6 (16a2c671): a native dispatch can carry its Linear key so the task and
// the issue are linked from birth.

func w6Dispatch(t *testing.T, h *Handlers, title, key string) (id, errMsg string) {
	t.Helper()
	args := map[string]any{"project": "p1", "as": "bot-a", "profile": "dev", "title": title}
	if key != "" {
		args["linear_key"] = key
	}
	res, _ := h.HandleDispatchTask(ctx, call(args))
	if res.IsError {
		return "", expectError(t, res)
	}
	return parseJSON(t, res)["task"].(map[string]any)["id"].(string), ""
}

func TestDispatchTask_StoresLinearKey(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	id, msg := w6Dispatch(t, h, "keyed work", "SYN-123")
	if msg != "" {
		t.Fatalf("dispatch with linear_key: %s", msg)
	}
	res, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": id}))
	got := parseJSON(t, res)
	if got["linear_key"] != "SYN-123" || got["source"] != "native" {
		t.Errorf("get_task linear_key=%v source=%v, want SYN-123 / native", got["linear_key"], got["source"])
	}
}

func TestDispatchTask_LinearKeyFormatRefused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	for _, bad := range []string{"syn-123", "SYN", "SYN-12a", "123-SYN", "SYN 123"} {
		if _, msg := w6Dispatch(t, h, "bad "+bad, bad); !strings.Contains(msg, "linear_key") {
			t.Errorf("linear_key %q: want a refusal naming linear_key, got %q", bad, msg)
		}
	}
}

func TestDispatchTask_LinearKeyHeldByActiveTaskRefused(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	first, msg := w6Dispatch(t, h, "first", "SYN-7")
	if msg != "" {
		t.Fatal(msg)
	}
	if _, msg := w6Dispatch(t, h, "second", "SYN-7"); !strings.Contains(msg, first) || !strings.Contains(msg, "linear_key") {
		t.Errorf("duplicate linear_key: want a refusal naming linear_key and task %s, got %q", first, msg)
	}
}

func TestBatchDispatch_LinearKeyPerItem(t *testing.T) {
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	items, _ := json.Marshal([]map[string]any{
		{"profile": "dev", "title": "with key", "linear_key": "SYN-9"},
		{"profile": "dev", "title": "without key"},
	})
	res, _ := h.HandleBatchDispatchTasks(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "tasks": string(items)}))
	out := parseJSON(t, res)
	done, _ := out["dispatched"].([]any)
	if len(done) != 2 {
		t.Fatalf("batch: dispatched %v errors %v, want 2 dispatched", out["dispatched"], out["errors"])
	}
	for _, d := range done {
		row := d.(map[string]any)
		task, err := h.db.GetTask(row["id"].(string), "p1")
		if err != nil || task == nil {
			t.Fatal(err)
		}
		want := ""
		if row["title"] == "with key" {
			want = "SYN-9"
		}
		if got := w6Str(task.LinearKey); got != want {
			t.Errorf("%v: linear_key %q, want %q", row["title"], got, want)
		}
	}
}

func w6Str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
