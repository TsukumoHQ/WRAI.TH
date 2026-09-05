package relay

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"agent-relay/internal/models"
)

// This suite covers the task-refusal observability seam (guardIdentity): every
// refused mutating task call leaves one INFO line and one audit_log row
// (action=task.refused) naming the caller identity, the project it sent, the
// task id and the typed code — so a caller-side "applied" claim can be checked
// against the relay's own record.

func regAgent(t *testing.T, h *Handlers, project, name string) {
	t.Helper()
	r, _ := h.HandleRegisterAgent(ctx, call(map[string]any{"project": project, "name": name, "role": "dev"}))
	if r.IsError {
		t.Fatalf("register %s in %s: %s", name, project, expectError(t, r))
	}
}

// dispatchClaimedIn dispatches a task in `project` to profile `doer` and claims
// it as `doer`, returning the task id (status=accepted, i.e. not blocked).
func dispatchClaimedIn(t *testing.T, h *Handlers, project, doer string) string {
	t.Helper()
	d, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": project, "as": "cto", "profile": doer, "title": "obs task",
	}))
	if d.IsError {
		t.Fatalf("dispatch: %s", expectError(t, d))
	}
	id := parseJSON(t, d)["task"].(map[string]any)["id"].(string)
	c, _ := h.HandleClaimTask(ctx, call(map[string]any{"project": project, "as": doer, "task_id": id}))
	if c.IsError {
		t.Fatalf("claim: %s", expectError(t, c))
	}
	return id
}

func refusedAuditRows(t *testing.T, h *Handlers, project, taskID string) []models.AuditEntry {
	t.Helper()
	rows, err := h.db.ListAudit(project, taskID, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var out []models.AuditEntry
	for _, r := range rows {
		if r.Action == "task.refused" {
			out = append(out, r)
		}
	}
	return out
}

// AC1: complete_task with a wrong project -> NOT_FOUND response unchanged AND
// one INFO line AND one audit_log row. Mirrors the incident: the caller (a doer)
// sent its OWN project, not the board's — identity passes in that project, but
// the task lookup misses, so the refusal is NOT_FOUND.
func TestTaskRefusal_CompleteWrongProject_LoggedAndAudited(t *testing.T) {
	h := testHandlers(t)
	regAgent(t, h, "p1", "cto")
	regAgent(t, h, "p1", "doer")
	regAgent(t, h, "p2", "doer") // doer's own home project — where it wrongly aimed
	taskID := dispatchClaimedIn(t, h, "p1", "doer")

	var logBuf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(old)

	guarded := h.guardIdentity("complete_task", h.HandleCompleteTask)
	res, _ := guarded(ctxWith("doer", "p2"), call(map[string]any{
		"project": "p2", "as": "doer", "task_id": taskID,
	}))

	// Refusal unchanged: the caller still gets NOT_FOUND.
	body := decodeToolError(t, res)
	if body["code"] != CodeNotFound {
		t.Fatalf("code = %v, want %s (refusal must be unchanged)", body["code"], CodeNotFound)
	}

	// One INFO line, exact format.
	want := "task-call refused tool=complete_task as=doer project=p2 task=" + taskID + " code=" + CodeNotFound
	if !strings.Contains(logBuf.String(), want) {
		t.Fatalf("missing INFO line\n  want substring: %s\n  got: %s", want, logBuf.String())
	}

	// Exactly one audit row, in the project the caller sent (p2), naming it.
	rows := refusedAuditRows(t, h, "p2", taskID)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 task.refused row in p2, got %d", len(rows))
	}
	r := rows[0]
	if r.Actor != "doer" || r.Project != "p2" || r.ResourceID != taskID {
		t.Errorf("audit row identity wrong: actor=%q project=%q resource=%q (want doer/p2/%s)", r.Actor, r.Project, r.ResourceID, taskID)
	}
	if !strings.HasPrefix(r.Reason, CodeNotFound+": ") {
		t.Errorf("reason = %q, want prefix %q", r.Reason, CodeNotFound+": ")
	}
}

// AC2: resume_task on an accepted (non-blocked) task -> refusal unchanged AND
// the same log line + audit row. The 'not blocked' refusal is identified by its
// message; the log and audit carry EXACTLY the code the caller received, so the
// relay's record never diverges from the response.
func TestTaskRefusal_ResumeNotBlocked_LoggedAndAudited(t *testing.T) {
	h := testHandlers(t)
	regAgent(t, h, "p1", "cto")
	regAgent(t, h, "p1", "doer")
	taskID := dispatchClaimedIn(t, h, "p1", "doer") // accepted, i.e. not blocked

	var logBuf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(old)

	guarded := h.guardIdentity("resume_task", h.HandleResumeTask)
	res, _ := guarded(ctxWith("doer", "p1"), call(map[string]any{
		"project": "p1", "as": "doer", "task_id": taskID,
	}))

	body := decodeToolError(t, res)
	code, _ := body["code"].(string)
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "not blocked") {
		t.Fatalf("expected a 'not blocked' refusal, got message %q", msg)
	}

	// INFO line carries the same code the caller received.
	want := "task-call refused tool=resume_task as=doer project=p1 task=" + taskID + " code=" + code
	if !strings.Contains(logBuf.String(), want) {
		t.Fatalf("missing INFO line\n  want substring: %s\n  got: %s", want, logBuf.String())
	}

	// One audit row; reason is exactly code + ": " + message (the 'not blocked'
	// refusal, recorded verbatim).
	rows := refusedAuditRows(t, h, "p1", taskID)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 task.refused row, got %d", len(rows))
	}
	if got, wantReason := rows[0].Reason, code+": "+msg; got != wantReason {
		t.Errorf("reason = %q, want %q", got, wantReason)
	}
}

// AC3: a SUCCESSFUL complete_task writes NO task.refused row (no false
// positives) — the seam only fires on an error result.
func TestTaskRefusal_SuccessWritesNoRow(t *testing.T) {
	h := testHandlers(t)
	regAgent(t, h, "p1", "cto")
	regAgent(t, h, "p1", "doer")
	taskID := dispatchClaimedIn(t, h, "p1", "doer")

	guarded := h.guardIdentity("complete_task", h.HandleCompleteTask)
	res, _ := guarded(ctxWith("doer", "p1"), call(map[string]any{
		"project": "p1", "as": "doer", "task_id": taskID,
	}))
	if res.IsError {
		t.Fatalf("complete in the right project should succeed: %s", expectError(t, res))
	}
	if rows := refusedAuditRows(t, h, "p1", taskID); len(rows) != 0 {
		t.Fatalf("a successful complete_task must write no task.refused row, got %d", len(rows))
	}
}
