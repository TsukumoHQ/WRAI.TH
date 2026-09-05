package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// expectArchived asserts an error result carries the typed ARCHIVED_PROJECT code
// (structured JSON, not a bare string) so a client can branch on it and PARK —
// the archived-project freeze is permission-category, non-retryable.
func expectArchived(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatal("expected a typed ARCHIVED_PROJECT error, got success/nil")
	}
	raw := res.Content[0].(mcp.TextContent).Text
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("refusal is not structured JSON (client can't branch on it): %v\nraw: %s", err, raw)
	}
	if body["code"] != "ARCHIVED_PROJECT" {
		t.Fatalf("want code ARCHIVED_PROJECT, got %v\nraw: %s", body["code"], raw)
	}
	if body["errorCategory"] != CategoryPermission {
		t.Fatalf("ARCHIVED_PROJECT must be permission-category (non-retryable): %v", body)
	}
}

// archivedProjectWithTask seeds project "p1" with a registered worker holding one
// claimed+started (non-terminal) task, then archives the project. Returns the
// task id so the allow-to-close tests can drive complete_task / update_task on
// real in-flight work. The seeding happens BEFORE the archive because dispatch is
// itself frozen once archived.
func archivedProjectWithTask(t *testing.T, h *Handlers) string {
	t.Helper()
	registerActive(t, h, "p1", "worker", nil)
	dispRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "profile": "dev", "title": "in-flight",
	}))
	taskID := parseJSON(t, dispRes)["task"].(map[string]any)["id"].(string)
	if r, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": "p1", "as": "worker", "task_id": taskID})); r.IsError {
		t.Fatalf("claim: %s", expectError(t, r))
	}
	if r, _ := h.HandleStartTask(ctx, call(map[string]any{"project": "p1", "as": "worker", "task_id": taskID})); r.IsError {
		t.Fatalf("start: %s", expectError(t, r))
	}
	if err := h.db.ArchiveProject("p1"); err != nil {
		t.Fatalf("archive p1: %v", err)
	}
	return taskID
}

// TestArchivedProjectRefusesRegisterDispatchSendClaim (AC1): once a project is
// archived, the four new-work tools — register_agent, dispatch_task,
// send_message, claim_task — are refused with the typed ARCHIVED_PROJECT code so
// the client parks. register_agent is a bootstrap tool (not guardIdentity-wrapped)
// and calls the guard from its own handler; the other three hit it at the seam.
func TestArchivedProjectRefusesRegisterDispatchSendClaim(t *testing.T) {
	h := testHandlers(t)
	taskID := archivedProjectWithTask(t, h)

	// register a NEW agent into the archived project → refused (direct handler).
	regRes, _ := h.HandleRegisterAgent(ctx, call(map[string]any{"project": "p1", "name": "latecomer", "role": "dev"}))
	expectArchived(t, regRes)

	dispatch := guardedHandler(t, h, "dispatch_task")
	dispRes, _ := dispatch(ctx, call(map[string]any{"project": "p1", "as": "worker", "profile": "dev", "title": "more work"}))
	expectArchived(t, dispRes)

	send := guardedHandler(t, h, "send_message")
	sendRes, _ := send(ctx, call(map[string]any{"project": "p1", "as": "worker", "to": "user", "content": "hi"}))
	expectArchived(t, sendRes)

	claim := guardedHandler(t, h, "claim_task")
	claimRes, _ := claim(ctx, call(map[string]any{"project": "p1", "as": "worker", "task_id": taskID}))
	expectArchived(t, claimRes)
}

// TestArchivedProjectAllowsCompleteAndUpdate (AC2): in-flight work can still be
// closed out. complete_task and update_task (without a reassign) on an existing
// non-terminal task in an archived project pass the guard (allow-to-close).
func TestArchivedProjectAllowsCompleteAndUpdate(t *testing.T) {
	h := testHandlers(t)
	taskID := archivedProjectWithTask(t, h)

	update := guardedHandler(t, h, "update_task")
	upRes, _ := update(ctx, call(map[string]any{"project": "p1", "as": "worker", "task_id": taskID, "title": "renamed in-flight"}))
	if upRes != nil && upRes.IsError {
		t.Fatalf("update_task without reassign must be allowed in an archived project: %s", upRes.Content[0].(mcp.TextContent).Text)
	}

	complete := guardedHandler(t, h, "complete_task")
	compRes, _ := complete(ctx, call(map[string]any{"project": "p1", "as": "worker", "task_id": taskID, "result": "done"}))
	if compRes != nil && compRes.IsError {
		t.Fatalf("complete_task must be allowed in an archived project: %s", compRes.Content[0].(mcp.TextContent).Text)
	}
}

// TestArchivedProjectRefusesReassign (AC3): an update_task carrying assigned_to
// is a reassignment — NEW work — so it is refused in an archived project even
// though a plain update_task is allowed.
func TestArchivedProjectRefusesReassign(t *testing.T) {
	h := testHandlers(t)
	taskID := archivedProjectWithTask(t, h)

	update := guardedHandler(t, h, "update_task")
	res, _ := update(ctx, call(map[string]any{"project": "p1", "as": "worker", "task_id": taskID, "assigned_to": "someone-else"}))
	expectArchived(t, res)
}

// TestArchivedProjectAllowsReads (AC4): reads never mutate, are absent from
// mutatingTools, and so never reach the guardIdentity seam — get_task on a task
// in an archived project still returns it.
func TestArchivedProjectAllowsReads(t *testing.T) {
	h := testHandlers(t)
	taskID := archivedProjectWithTask(t, h)

	getRes, _ := h.HandleGetTask(ctx, call(map[string]any{"project": "p1", "task_id": taskID}))
	if getRes.IsError {
		t.Fatalf("get_task must be allowed in an archived project: %s", getRes.Content[0].(mcp.TextContent).Text)
	}
	if got := parseJSON(t, getRes)["title"]; got != "in-flight" {
		t.Fatalf("read returned wrong task: title=%v", got)
	}
}

// TestDeleteProjectRequiresArchivedFirst (AC5a): delete_project on an ACTIVE
// project is refused — purge requires archived (DEC-wraith-archive-project-1 §2).
func TestDeleteProjectRequiresArchivedFirst(t *testing.T) {
	h := testHandlers(t)
	h.db.EnsureProject("p1")

	res, _ := h.HandleDeleteProject(ctx, call(map[string]any{"project": "p1"}))
	msg := expectError(t, res)
	if !strings.Contains(msg, "archive_project it first") {
		t.Fatalf("want 'archive first' refusal, got: %s", msg)
	}
}

// TestDeleteProjectPurgesArchived (AC5b): once archived, delete_project performs
// the purge (existing delete behaviour) and succeeds.
func TestDeleteProjectPurgesArchived(t *testing.T) {
	h := testHandlers(t)
	h.db.EnsureProject("p1")
	if err := h.db.ArchiveProject("p1"); err != nil {
		t.Fatalf("archive p1: %v", err)
	}

	res, _ := h.HandleDeleteProject(ctx, call(map[string]any{"project": "p1"}))
	if res.IsError {
		t.Fatalf("delete_project on an archived project must purge, got error: %s", expectError(t, res))
	}
	if h.db.IsProjectArchived("p1") {
		t.Fatal("project should no longer exist after purge")
	}
}
