package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-relay/internal/db"
)

// update_task parent_task_id (task eae0243b): a dispatcher regroups live
// tickets under an epic after dispatch, or detaches them with "".

func parentFixture(t *testing.T) *Handlers {
	t.Helper()
	h := testHandlers(t)
	registerActive(t, h, "p1", "lead", nil)
	registerActive(t, h, "p1", "dev-x", nil)
	registerActive(t, h, "p2", "lead", nil)
	return h
}

// dispatchParentTest dispatches a free-form task as `as` and returns its id.
func dispatchParentTest(t *testing.T, h *Handlers, project, as, title string) string {
	t.Helper()
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{"project": project, "as": as, "profile": "dev", "title": title}))
	if res.IsError {
		t.Fatalf("dispatch %q: %s", title, resultText(res))
	}
	// The first dispatch in a project wraps the task ({"task":..., "auto_board":...}).
	var got struct {
		ID   string `json:"id"`
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal([]byte(resultText(res)), &got); err != nil {
		t.Fatalf("dispatch %q: %v in %s", title, err, resultText(res))
	}
	if got.Task.ID != "" {
		return got.Task.ID
	}
	if got.ID == "" {
		t.Fatalf("dispatch %q: no id in %s", title, resultText(res))
	}
	return got.ID
}

func setParent(h *Handlers, project, as, taskID, parent string) (string, bool) {
	res, _ := h.HandleUpdateTask(ctx, call(map[string]any{"project": project, "as": as, "task_id": taskID, "parent_task_id": parent}))
	return resultText(res), res.IsError
}

func subtaskIDs(t *testing.T, h *Handlers, project, parent string) []string {
	t.Helper()
	res, _ := h.HandleGetTask(ctx, call(map[string]any{"project": project, "task_id": parent, "include_subtasks": true}))
	if res.IsError {
		t.Fatalf("get_task %s: %s", parent, resultText(res))
	}
	var got struct {
		Subtasks []struct {
			ID string `json:"id"`
		} `json:"subtasks"`
	}
	if err := json.Unmarshal([]byte(resultText(res)), &got); err != nil {
		t.Fatalf("decode get_task: %v", err)
	}
	ids := make([]string, 0, len(got.Subtasks))
	for _, s := range got.Subtasks {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestUpdateTaskParent_SetListsAndClear(t *testing.T) {
	h := parentFixture(t)
	epic := dispatchParentTest(t, h, "p1", "lead", "epic")
	child := dispatchParentTest(t, h, "p1", "lead", "child")

	if msg, isErr := setParent(h, "p1", "lead", child, epic); isErr {
		t.Fatalf("set parent: %s", msg)
	}
	if ids := subtaskIDs(t, h, "p1", epic); len(ids) != 1 || ids[0] != child {
		t.Fatalf("epic subtasks = %v, want [%s]", ids, child)
	}
	task, _ := h.db.GetTask(child, "p1")
	if task.ParentTaskID == nil || *task.ParentTaskID != epic {
		t.Fatalf("child parent_task_id = %v, want %s", task.ParentTaskID, epic)
	}

	if msg, isErr := setParent(h, "p1", "lead", child, ""); isErr {
		t.Fatalf("clear parent: %s", msg)
	}
	if ids := subtaskIDs(t, h, "p1", epic); len(ids) != 0 {
		t.Fatalf("after clear, epic subtasks = %v, want none", ids)
	}
	task, _ = h.db.GetTask(child, "p1")
	if task.ParentTaskID != nil && *task.ParentTaskID != "" {
		t.Fatalf("after clear, parent_task_id = %q", *task.ParentTaskID)
	}
}

func TestUpdateTaskParent_RefusesSelfCycleAndOtherProject(t *testing.T) {
	h := parentFixture(t)
	a := dispatchParentTest(t, h, "p1", "lead", "a")
	b := dispatchParentTest(t, h, "p1", "lead", "b")
	c := dispatchParentTest(t, h, "p1", "lead", "c")
	other := dispatchParentTest(t, h, "p2", "lead", "elsewhere")

	// a <- b <- c (c's parent is b, b's parent is a)
	for _, e := range [][2]string{{b, a}, {c, b}} {
		if msg, isErr := setParent(h, "p1", "lead", e[0], e[1]); isErr {
			t.Fatalf("seed chain: %s", msg)
		}
	}

	for _, tc := range []struct {
		name, task, parent, want string
	}{
		{"self-parent", a, a, "INVALID_ARGUMENT"},
		{"direct cycle", b, c, "INVALID_ARGUMENT"},
		{"cycle through the chain", a, c, "INVALID_ARGUMENT"},
		{"parent in another project", a, other, "NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, isErr := setParent(h, "p1", "lead", tc.task, tc.parent)
			if !isErr || !strings.Contains(msg, tc.want) {
				t.Fatalf("want %s refusal, got isErr=%v %s", tc.want, isErr, msg)
			}
		})
	}
	// Refusals wrote nothing: a stays a root.
	if task, _ := h.db.GetTask(a, "p1"); task.ParentTaskID != nil && *task.ParentTaskID != "" {
		t.Fatalf("a refused edit changed parent to %q", *task.ParentTaskID)
	}
}

func TestUpdateTaskParent_NonDispatcherRefusedAndAudited(t *testing.T) {
	h := parentFixture(t)
	epic := dispatchParentTest(t, h, "p1", "lead", "epic")
	child := dispatchParentTest(t, h, "p1", "lead", "child")

	msg, isErr := setParent(h, "p1", "dev-x", child, epic)
	if !isErr || !strings.Contains(msg, "FORBIDDEN") {
		t.Fatalf("non-dispatcher: want FORBIDDEN, got isErr=%v %s", isErr, msg)
	}
	if task, _ := h.db.GetTask(child, "p1"); task.ParentTaskID != nil && *task.ParentTaskID != "" {
		t.Fatalf("refused edit changed parent to %q", *task.ParentTaskID)
	}

	if msg, isErr := setParent(h, "p1", "lead", child, epic); isErr {
		t.Fatalf("dispatcher set parent: %s", msg)
	}
	notes, err := h.db.GetProgressNotes(child, "p1")
	if err != nil {
		t.Fatalf("progress notes: %v", err)
	}
	want := "parent set to " + epic + " by lead"
	found := false
	for _, n := range notes {
		if n.Note == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit note %q in %+v", want, notes)
	}
}

func TestUpdateTaskParent_ShortIDAndUnknown(t *testing.T) {
	h := parentFixture(t)
	epic := dispatchParentTest(t, h, "p1", "lead", "epic")
	child := dispatchParentTest(t, h, "p1", "lead", "child")

	if msg, isErr := setParent(h, "p1", "lead", child, epic[:8]); isErr {
		t.Fatalf("short-id parent: %s", msg)
	}
	if task, _ := h.db.GetTask(child, "p1"); task.ParentTaskID == nil || *task.ParentTaskID != epic {
		t.Fatalf("short id did not resolve to %s: %v", epic, task.ParentTaskID)
	}

	msg, isErr := setParent(h, "p1", "lead", child, "00000000-0000-0000-0000-000000000000")
	if !isErr || !strings.Contains(msg, "NOT_FOUND") {
		t.Fatalf("unknown parent: want NOT_FOUND, got isErr=%v %s", isErr, msg)
	}
}

// A Linear mirror's parent syncs from Linear (linear.go upsert); a local
// update_task edit would be overwritten on the next sync, so it is refused.
func TestUpdateTaskParent_LinearMirrorRefused(t *testing.T) {
	h := parentFixture(t)
	epic := dispatchParentTest(t, h, "p1", "lead", "epic")
	if err := h.db.UpsertLinearTask(db.LinearTaskSeed{ID: "lt-1", Project: "p1", Title: "mirror", Status: "pending"}); err != nil {
		t.Fatalf("seed linear task: %v", err)
	}
	msg, isErr := setParent(h, "p1", "lead", "lt-1", epic)
	if !isErr || !strings.Contains(msg, "Linear") {
		t.Fatalf("linear mirror: want refusal naming Linear, got isErr=%v %s", isErr, msg)
	}
	if task, _ := h.db.GetTask("lt-1", "p1"); task.ParentTaskID != nil && *task.ParentTaskID != "" {
		t.Fatalf("refused edit changed parent to %q", *task.ParentTaskID)
	}
}
