package relay

import (
	"testing"
)

// park P5 e6ee47b4 — groomed backlog work is listable on its own and never
// mixed with claimable work (design docs/design/park.md, ruling
// wraith-park-ruling).

// backlogFleet: one task dispatched straight to backlog, one pending.
func backlogFleet(t *testing.T) (h *Handlers, backlogID, pendingID string) {
	t.Helper()
	h = demoteFleet(t)
	res, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "lead", "profile": "dev", "title": "groomed", "backlog": true,
	}))
	if res.IsError {
		t.Fatalf("dispatch backlog: %s", expectError(t, res))
	}
	backlogID = parseJSON(t, res)["task"].(map[string]any)["id"].(string)
	if got := taskStatus(t, h, backlogID); got != "backlog" {
		t.Fatalf("backlog dispatch: status %s, want backlog", got)
	}
	pendingID = demoteDispatch(t, h, "claimable")
	return h, backlogID, pendingID
}

func listTaskIDs(t *testing.T, h *Handlers, args map[string]any) map[string]string {
	t.Helper()
	args["project"] = "p1"
	args["format"] = "json"
	res, _ := h.HandleListTasks(ctx, call(args))
	if res.IsError {
		t.Fatalf("list_tasks %v: %s", args, expectError(t, res))
	}
	out := map[string]string{}
	for _, raw := range parseJSON(t, res)["tasks"].([]any) {
		task := raw.(map[string]any)
		out[task["id"].(string)] = task["status"].(string)
	}
	return out
}

// AC1 — list_tasks status='backlog' returns only backlog tasks, and the tool
// schema offers 'backlog' as a status value.
func TestListTasksStatusBacklogOnlyBacklog(t *testing.T) {
	h, backlogID, pendingID := backlogFleet(t)

	got := listTaskIDs(t, h, map[string]any{"status": "backlog"})
	if _, ok := got[backlogID]; !ok || len(got) != 1 {
		t.Fatalf("status=backlog: got %v, want only %s", got, backlogID)
	}
	if _, ok := got[pendingID]; ok {
		t.Fatalf("status=backlog returned the pending task %s", pendingID)
	}

	enum := listTasksTool().InputSchema.Properties["status"].(map[string]any)["enum"]
	found := false
	for _, v := range enum.([]string) {
		found = found || v == "backlog"
	}
	if !found {
		t.Fatalf("list_tasks status enum %v lacks 'backlog'", enum)
	}
}

// AC2 — list_tasks ready=true never returns a backlog task.
func TestListTasksReadyNeverBacklog(t *testing.T) {
	h, backlogID, pendingID := backlogFleet(t)

	got := listTaskIDs(t, h, map[string]any{"ready": true})
	if _, ok := got[backlogID]; ok {
		t.Fatalf("ready=true returned backlog task %s: %v", backlogID, got)
	}
	if _, ok := got[pendingID]; !ok {
		t.Fatalf("ready=true lost the pending task %s: %v", pendingID, got)
	}
	// status does not override ready: still no backlog task.
	got = listTaskIDs(t, h, map[string]any{"ready": true, "status": "backlog"})
	if _, ok := got[backlogID]; ok {
		t.Fatalf("ready=true status=backlog returned backlog task %s", backlogID)
	}
}
