package relay

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// dispatch_task parent_task_id (task 98be27bb): a short parent prefix used to be
// stored verbatim (dangling parent) and the subtask landed on another board;
// blocked_by sent as a JSON string was silently dropped.

type dispatchedTask struct {
	ID           string  `json:"id"`
	ParentTaskID *string `json:"parent_task_id"`
	BoardID      *string `json:"board_id"`
	BlockedBy    []struct {
		ID string `json:"id"`
	} `json:"blocked_by"`
}

func dispatchArgs(t *testing.T, h *Handlers, args map[string]any) (dispatchedTask, string, bool) {
	t.Helper()
	full := map[string]any{"project": "p1", "as": "lead", "profile": "dev"}
	for k, v := range args {
		full[k] = v
	}
	res, _ := h.HandleDispatchTask(ctx, call(full))
	text := resultText(res)
	if res.IsError {
		return dispatchedTask{}, text, true
	}
	var got struct {
		Task dispatchedTask `json:"task"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil || got.Task.ID == "" {
		t.Fatalf("dispatch: no task in %s", text)
	}
	return got.Task, text, false
}

func parentDispatchFixture(t *testing.T) (*Handlers, string, string) {
	t.Helper()
	h := testHandlers(t)
	registerActive(t, h, "p1", "lead", nil)
	a, err := h.db.CreateBoard("p1", "Alpha", "alpha", "", "lead")
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	b, err := h.db.CreateBoard("p1", "Beta", "beta", "", "lead")
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	return h, a.ID, b.ID
}

func TestDispatchParent_ShortPrefixStoresFullID(t *testing.T) {
	h, _, beta := parentDispatchFixture(t)
	parent, msg, isErr := dispatchArgs(t, h, map[string]any{"title": "epic", "board_id": beta})
	if isErr {
		t.Fatalf("dispatch parent: %s", msg)
	}

	child, msg, isErr := dispatchArgs(t, h, map[string]any{"title": "child", "parent_task_id": parent.ID[:8], "board_id": beta})
	if isErr {
		t.Fatalf("dispatch child with prefix: %s", msg)
	}
	if child.ParentTaskID == nil || *child.ParentTaskID != parent.ID {
		t.Fatalf("response parent_task_id = %v, want full %s", child.ParentTaskID, parent.ID)
	}
	stored, _ := h.db.GetTask(child.ID, "p1")
	if stored.ParentTaskID == nil || *stored.ParentTaskID != parent.ID {
		t.Fatalf("stored parent_task_id = %v, want full %s", stored.ParentTaskID, parent.ID)
	}
	if ids := subtaskIDs(t, h, "p1", parent.ID); len(ids) != 1 || ids[0] != child.ID {
		t.Fatalf("parent subtasks = %v, want [%s]", ids, child.ID)
	}

	_, msg, isErr = dispatchArgs(t, h, map[string]any{"title": "orphan", "parent_task_id": "deadbeef", "board_id": beta})
	if !isErr || !strings.Contains(msg, "NOT_FOUND") {
		t.Fatalf("unknown parent prefix: want NOT_FOUND, got isErr=%v %s", isErr, msg)
	}
}

func TestDispatchParent_SubtaskInheritsParentBoard(t *testing.T) {
	h, _, beta := parentDispatchFixture(t)
	parent, msg, isErr := dispatchArgs(t, h, map[string]any{"title": "epic", "board_id": beta})
	if isErr {
		t.Fatalf("dispatch parent: %s", msg)
	}
	// Two boards and no board_id: without the parent this is ambiguous (refused
	// or product-routed); a subtask must follow its parent.
	child, msg, isErr := dispatchArgs(t, h, map[string]any{"title": "child", "parent_task_id": parent.ID[:8]})
	if isErr {
		t.Fatalf("dispatch child without board: %s", msg)
	}
	if child.BoardID == nil || *child.BoardID != beta {
		t.Fatalf("child board = %v, want parent's board %s", child.BoardID, beta)
	}

	// An explicit board_id still wins over the parent's.
	alpha, _ := h.db.ListBoards("p1")
	var alphaID string
	for _, b := range alpha {
		if b.Slug == "alpha" {
			alphaID = b.ID
		}
	}
	explicit, msg, isErr := dispatchArgs(t, h, map[string]any{"title": "explicit", "parent_task_id": parent.ID, "board_id": alphaID})
	if isErr {
		t.Fatalf("dispatch with explicit board: %s", msg)
	}
	if explicit.BoardID == nil || *explicit.BoardID != alphaID {
		t.Fatalf("explicit board = %v, want %s", explicit.BoardID, alphaID)
	}
}

func TestDispatchParent_BlockedByPersistedAndReturned(t *testing.T) {
	h, _, beta := parentDispatchFixture(t)
	pre, msg, isErr := dispatchArgs(t, h, map[string]any{"title": "prerequisite", "board_id": beta})
	if isErr {
		t.Fatalf("dispatch prerequisite: %s", msg)
	}

	for _, tc := range []struct {
		name string
		arg  any
	}{
		{"native array", []any{pre.ID[:8]}},
		{"JSON-array string", `["` + pre.ID[:8] + `"]`},
		{"plain string", pre.ID[:8]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, msg, isErr := dispatchArgs(t, h, map[string]any{"title": "blocked " + tc.name, "board_id": beta, "blocked_by": tc.arg})
			if isErr {
				t.Fatalf("dispatch: %s", msg)
			}
			if len(task.BlockedBy) != 1 || task.BlockedBy[0].ID != pre.ID {
				t.Fatalf("response blocked_by = %+v, want [%s]", task.BlockedBy, pre.ID)
			}
			if ready, refs, _ := h.db.TaskReadiness("p1", task.ID); ready || len(refs) != 1 || refs[0].ID != pre.ID {
				t.Fatalf("persisted: ready=%v refs=%+v, want blocked by %s", ready, refs, pre.ID)
			}
		})
	}

	_, msg, isErr = dispatchArgs(t, h, map[string]any{"title": "bad blocked_by", "board_id": beta, "blocked_by": 42})
	if !isErr || !strings.Contains(msg, "INVALID_ARGUMENT") {
		t.Fatalf("wrong-typed blocked_by: want INVALID_ARGUMENT, got isErr=%v %s", isErr, msg)
	}
}

// The REST dispatch path funnels through db.DispatchTask, which resolves the
// prefix too — the board UI / scripts get the same parent link.
func TestDispatchParent_RESTResolvesPrefix(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/tasks", `{"project":"p1","dispatched_by":"bot-a","profile":"dev","title":"epic"}`)
	if w.Code >= 300 {
		t.Fatalf("REST parent dispatch: %d %s", w.Code, w.Body.String())
	}
	var parent dispatchedTask
	if err := json.Unmarshal(w.Body.Bytes(), &parent); err != nil || parent.ID == "" {
		t.Fatalf("decode parent: %v %s", err, w.Body.String())
	}
	w = doAPI(r, "POST", "/tasks", `{"project":"p1","dispatched_by":"bot-a","profile":"dev","title":"child","parent_task_id":"`+parent.ID[:8]+`"}`)
	if w.Code >= 300 {
		t.Fatalf("REST child dispatch: %d %s", w.Code, w.Body.String())
	}
	var child dispatchedTask
	_ = json.Unmarshal(w.Body.Bytes(), &child)
	if child.ParentTaskID == nil || *child.ParentTaskID != parent.ID {
		t.Fatalf("REST child parent = %v, want %s", child.ParentTaskID, parent.ID)
	}

	w = doAPI(r, "POST", "/tasks", `{"project":"p1","dispatched_by":"bot-a","profile":"dev","title":"orphan","parent_task_id":"deadbeef"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("REST unknown parent: %d %s, want 404", w.Code, w.Body.String())
	}
}
