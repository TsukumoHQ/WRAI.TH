package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"agent-relay/internal/db"
	"agent-relay/internal/models"
)

// HandleParkTask parks a pending task (task d43d844e): held on purpose, so no
// ACK rung ever pages anyone about it. until=founder makes it a founder gate
// (alerted once, now); until=[task:]<id>[@in-review] unparks it when that task
// is done (or in review). Only the dispatcher, an executive or the doer's lead
// chain may park.
func (h *Handlers) HandleParkTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	task, res := h.parkTarget(project, agent, req.GetString("task_id", ""), "park")
	if res != nil {
		return res, nil
	}
	out, res := h.park(project, agent, task, req.GetString("reason", ""), req.GetString("until", ""))
	if res != nil {
		return res, nil
	}
	return h.resultJSONTracked(project, agent, "park_task", out)
}

// park parks task for agent (authority already checked) and tells the
// dispatcher once per park (task fea65594): a re-park with the same reason is
// silent. Shared by park_task and POST /api/tasks/{id}/park.
func (h *Handlers) park(project, agent string, task *models.Task, reason, until string) (map[string]any, *mcp.CallToolResult) {
	reason, until = strings.TrimSpace(reason), strings.TrimSpace(until)
	if reason == "" || until == "" {
		return nil, validationError(CodeInvalidArgument, "reason and until (founder or a task id) are required")
	}
	untilStatus := ""
	if strings.EqualFold(until, db.ParkUntilFounder) {
		until = db.ParkUntilFounder
	} else {
		if len(until) > 5 && strings.EqualFold(until[:5], "task:") {
			until = until[5:]
		}
		id, status, _ := strings.Cut(until, "@")
		full, err := h.resolveTaskID(id, project)
		if err != nil {
			return nil, toolResultError(fmt.Sprintf("until: %v", err))
		}
		until, untilStatus = full, status
	}
	fresh, err := h.db.ParkTask(project, task.ID, agent, reason, until, untilStatus)
	if err != nil {
		return nil, taskOpError(err, "failed to park task: %v", err)
	}
	if until == db.ParkUntilFounder {
		evaluateFounderGates(h.db, h.registry, h.db.Now())
	}
	if fresh {
		h.notifyParked(project, agent, task, reason, until, untilStatus)
	}
	details, _ := json.Marshal(map[string]string{"until": until, "until_status": untilStatus})
	_ = h.db.RecordAudit(models.AuditEntry{Project: project, Actor: agent, Action: "task.parked", ResourceType: "task",
		ResourceID: task.ID, Summary: fmt.Sprintf("parked %q until %s", task.Title, until), Details: string(details), Reason: reason})
	h.events.Emit(MCPEvent{Type: "task", Action: "parked", Agent: agent, Project: project, Label: task.Title})
	out := map[string]any{"task_id": task.ID, "parked": true, "until": until, "reason": reason}
	if untilStatus != "" {
		out["until_status"] = untilStatus
	}
	return out, nil
}

// notifyParked is the one no-wake fyi a park sends the task's dispatcher.
func (h *Handlers) notifyParked(project, agent string, task *models.Task, reason, until, untilStatus string) {
	if untilStatus != "" {
		until += " " + untilStatus
	}
	text := fmt.Sprintf("PARKED: task '%s' (%s) until %s by %s — %s", task.Title, task.ID, until, agent, reason)
	meta := fmt.Sprintf(`{"task_id":%q,"alert":"parked"}`, task.ID)
	msg, _, err := h.db.InsertMessageWithDeliveries(project, "relay", task.DispatchedBy, "fyi", text, text, meta,
		"P2", -1, nil, nil, []string{task.DispatchedBy}, "none")
	if err != nil {
		log.Printf("park notify error: task %s: %v", task.ID, err)
		return
	}
	h.registry.Notify(project, task.DispatchedBy, "relay", text, msg.ID)
}

// unpark lifts a park (resume_task on a parked task); a hold it released is
// announced claimable.
func (h *Handlers) unpark(project, agent string, task *models.Task) (*mcp.CallToolResult, error) {
	out, res := h.unparkTask(project, agent, task)
	if res != nil {
		return res, nil
	}
	return h.resultJSONTracked(project, agent, "resume_task", out)
}

// unparkTask is unpark's core, shared with POST /api/tasks/{id}/unpark.
func (h *Handlers) unparkTask(project, agent string, task *models.Task) (map[string]any, *mcp.CallToolResult) {
	// The audit names the park being lifted (unpark itself takes no reason).
	lifted := ""
	one := []models.Task{*task}
	if err := h.db.AttachParks(project, one); err == nil && one[0].Park != nil {
		lifted = one[0].Park.Reason
	}
	released, err := h.db.UnparkTask(project, task.ID, agent)
	if err != nil {
		return nil, taskOpError(err, "failed to unpark task: %v", err)
	}
	_ = h.db.RecordAudit(models.AuditEntry{Project: project, Actor: agent, Action: "task.unparked", ResourceType: "task",
		ResourceID: task.ID, Summary: fmt.Sprintf("unparked %q", task.Title), Reason: lifted})
	if released {
		h.announceRelease(project, task.ID)
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "unparked", Agent: agent, Project: project, Label: task.Title})
	return map[string]any{"task_id": task.ID, "parked": false, "released": released}, nil
}

// apiParkTask parks a pending task from the operator console (task fea65594).
// Path: POST /api/tasks/{id}/park {project, reason, until}. The operator acts
// as "user", like the other console task writes (reassign, requeue).
func (r *Relay) apiParkTask(w http.ResponseWriter, req *http.Request, taskID string) {
	var body struct {
		Reason string `json:"reason"`
		Until  string `json:"until"`
	}
	project, task, ok := r.apiParkBody(w, req, taskID, &body)
	if !ok {
		return
	}
	out, res := r.Handlers.park(project, "user", task, body.Reason, body.Until)
	if res != nil {
		apiError(w, http.StatusBadRequest, "failed to park task", errors.New(toolText(res)))
		return
	}
	writeJSON(w, out)
}

// apiUnparkTask lifts a park from the operator console.
// Path: POST /api/tasks/{id}/unpark {project}.
func (r *Relay) apiUnparkTask(w http.ResponseWriter, req *http.Request, taskID string) {
	project, task, ok := r.apiParkBody(w, req, taskID, &struct{}{})
	if !ok {
		return
	}
	out, res := r.Handlers.unparkTask(project, "user", task)
	if res != nil {
		apiError(w, http.StatusBadRequest, "failed to unpark task", errors.New(toolText(res)))
		return
	}
	writeJSON(w, out)
}

// apiParkBody decodes a park/unpark body into body plus its "project" (default
// "default") and loads the task; false means a response was written.
func (r *Relay) apiParkBody(w http.ResponseWriter, req *http.Request, taskID string, body any) (string, *models.Task, bool) {
	raw, err := io.ReadAll(req.Body)
	if err != nil || json.Unmarshal(raw, body) != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return "", nil, false
	}
	var p struct {
		Project string `json:"project"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.Project == "" {
		p.Project = "default"
	}
	full, err := r.Handlers.resolveTaskID(taskID, p.Project)
	if err != nil {
		apiError(w, http.StatusNotFound, "task not found", err)
		return "", nil, false
	}
	task, err := r.DB.GetTask(full, p.Project)
	if err != nil || task == nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return "", nil, false
	}
	return p.Project, task, true
}

// toolText is the text of a tool result (a refusal's message).
func toolText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// parkTarget resolves the task of a park/unpark and checks the caller may act.
func (h *Handlers) parkTarget(project, agent, rawID, verb string) (*models.Task, *mcp.CallToolResult) {
	if rawID == "" {
		return nil, validationError(CodeInvalidArgument, "task_id is required")
	}
	taskID, err := h.resolveTaskID(rawID, project)
	if err != nil {
		return nil, toolResultError(err.Error())
	}
	task, err := h.db.GetTask(taskID, project)
	if err != nil || task == nil {
		return nil, typedTaskError(&db.TaskError{Code: db.CodeTaskNotFound, Msg: fmt.Sprintf("task %s not found in project %s", taskID, project)})
	}
	if !h.callerMayReassign(project, agent, task) && !h.callerLeadsMirrorLane(project, agent, task) {
		return nil, permissionError(CodeForbidden, fmt.Sprintf(
			"only the dispatcher (%s), an executive, the doer's lead or (Linear mirror) the routed lane lead and its chain may %s task %s", task.DispatchedBy, verb, taskID))
	}
	return task, nil
}

// callerLeadsMirrorLane reports whether caller is the lead a Linear mirror
// routes to (its profile_slug, the resolved dispatch target) or an agent in
// that lead's reports_to chain (ruling wraith-park-ruling Q1): the lane owner
// freezes its own queue without an executive. Bounded by a seen-set against a
// cyclic chain; agent names fold case.
func (h *Handlers) callerLeadsMirrorLane(project, caller string, task *models.Task) bool {
	if caller == "" || task.Source != "linear" {
		return false
	}
	cur := strings.ToLower(task.ProfileSlug)
	seen := map[string]bool{}
	for cur != "" && !seen[cur] {
		if strings.EqualFold(cur, caller) {
			return true
		}
		seen[cur] = true
		ag, err := h.db.GetAgent(project, cur)
		if err != nil || ag == nil || ag.ReportsTo == nil {
			return false
		}
		cur = strings.ToLower(*ag.ReportsTo)
	}
	return false
}

// updateBlockedBy applies update_task's blocked_by / blocked_by_remove (task
// d43d844e): dispatcher-only edits of the prerequisites after dispatch, through
// the same edge writes as task_edge. Returns a refusal to hand back, or nil.
func (h *Handlers) updateBlockedBy(project, agent, taskID string, add, remove []string) *mcp.CallToolResult {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	task, err := h.db.GetTask(taskID, project)
	if err != nil || task == nil {
		return toolResultError(fmt.Sprintf("task not found: %s", taskID))
	}
	if !strings.EqualFold(agent, task.DispatchedBy) {
		return permissionError(CodeForbidden, fmt.Sprintf(
			"blocked_by can only be edited by this task's dispatcher (%s)", task.DispatchedBy))
	}
	for _, raw := range add {
		id, until, _ := strings.Cut(strings.TrimSpace(raw), "@")
		if id == "" {
			continue
		}
		full, err := h.resolveTaskID(id, project)
		if err != nil {
			return toolResultError(fmt.Sprintf("blocked_by: %v", err))
		}
		meta := map[string]any{}
		if until != "" {
			meta["until"] = until
		}
		if err := h.db.AddEdge(project, db.EdgeInput{SrcKind: "task", SrcID: taskID, Type: db.EdgeBlockedBy,
			DstKind: "task", DstID: full, Metadata: meta, CreatedBy: agent}); err != nil {
			return taskOpError(err, "blocked_by: %v", err)
		}
	}
	for _, raw := range remove {
		id, _, _ := strings.Cut(strings.TrimSpace(raw), "@")
		if id == "" {
			continue
		}
		full, err := h.resolveTaskID(id, project)
		if err != nil {
			return toolResultError(fmt.Sprintf("blocked_by_remove: %v", err))
		}
		released, err := h.db.RemoveEdge(project, taskID, db.EdgeBlockedBy, full, agent)
		if err != nil {
			return taskOpError(err, "blocked_by_remove: %v", err)
		}
		h.announceReleased(project, released)
	}
	return nil
}
