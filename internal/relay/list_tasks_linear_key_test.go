package relay

import "testing"

// W11 (c4f65348): finding one task by its Linear key costs one small
// response, not the whole board — list_tasks filters by linear_key in SQL.

func w11Fixture(t *testing.T) (*Handlers, map[string]string) {
	t.Helper()
	h := testHandlers(t)
	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "bot-a"}))
	ids := map[string]string{}
	for title, key := range map[string]string{"one": "SYN-1", "two": "SYN-123", "three": ""} {
		res, _ := h.HandleDispatchTask(ctx, call(map[string]any{"project": "p1", "as": "bot-a", "profile": "dev", "title": title}))
		id := parseJSON(t, res)["task"].(map[string]any)["id"].(string)
		if key != "" {
			if err := h.db.LinkTaskLinear(id, "iss-"+key, key); err != nil {
				t.Fatal(err)
			}
		}
		ids[title] = id
	}
	return h, ids
}

func w11List(t *testing.T, h *Handlers, args map[string]any) []any {
	t.Helper()
	args["project"], args["format"] = "p1", "json"
	res, _ := h.HandleListTasks(ctx, call(args))
	if res.IsError {
		t.Fatalf("list_tasks %v: %s", args, expectError(t, res))
	}
	tasks, _ := parseJSON(t, res)["tasks"].([]any)
	return tasks
}

func TestListTasks_LinearKeyFilter(t *testing.T) {
	h, ids := w11Fixture(t)
	got := w11List(t, h, map[string]any{"linear_key": "SYN-123"})
	if len(got) != 1 || got[0].(map[string]any)["id"] != ids["two"] {
		t.Fatalf("linear_key SYN-123: got %d task(s) %v, want exactly task two", len(got), got)
	}
}

func TestListTasks_UnknownLinearKeyReturnsNone(t *testing.T) {
	h, _ := w11Fixture(t)
	if got := w11List(t, h, map[string]any{"linear_key": "SYN-999"}); len(got) != 0 {
		t.Fatalf("unknown linear_key: got %d task(s), want 0 (not the unfiltered list)", len(got))
	}
}

func TestListTasks_LinearKeyAndStatus(t *testing.T) {
	h, ids := w11Fixture(t)
	if _, err := h.db.CancelTask(ids["two"], "bot-a", "p1", nil); err != nil {
		t.Fatal(err)
	}
	if got := w11List(t, h, map[string]any{"linear_key": "SYN-123", "status": "pending"}); len(got) != 0 {
		t.Errorf("linear_key AND status=pending on a cancelled task: got %d, want 0", len(got))
	}
	if got := w11List(t, h, map[string]any{"linear_key": "SYN-123", "status": "cancelled"}); len(got) != 1 {
		t.Errorf("linear_key AND status=cancelled: got %d, want 1", len(got))
	}
}
