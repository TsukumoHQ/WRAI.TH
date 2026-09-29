package relay

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"agent-relay/internal/db"
	"agent-relay/internal/models"
)

// HandleParkTask parks a pending task (task d43d844e): held on purpose, so no
// ACK rung ever pages anyone about it. until=founder makes it a founder gate
// (alerted once, now); until=<task id> unparks it when that task is done. Only
// the dispatcher, an executive or the doer's lead chain may park.
func (h *Handlers) HandleParkTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	reason := strings.TrimSpace(req.GetString("reason", ""))
	until := strings.TrimSpace(req.GetString("until", ""))
	if reason == "" || until == "" {
		return validationError(CodeInvalidArgument, "reason and until (founder or a task id) are required"), nil
	}
	task, res := h.parkTarget(project, agent, req.GetString("task_id", ""), "park")
	if res != nil {
		return res, nil
	}
	if !strings.EqualFold(until, db.ParkUntilFounder) {
		full, err := h.resolveTaskID(until, project)
		if err != nil {
			return toolResultError(fmt.Sprintf("until: %v", err)), nil
		}
		until = full
	} else {
		until = db.ParkUntilFounder
	}
	if err := h.db.ParkTask(project, task.ID, agent, reason, until); err != nil {
		return taskOpError(err, "failed to park task: %v", err), nil
	}
	if until == db.ParkUntilFounder {
		evaluateFounderGates(h.db, h.registry, h.db.Now())
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "parked", Agent: agent, Project: project, Label: task.Title})
	return h.resultJSONTracked(project, agent, "park_task", map[string]any{
		"task_id": task.ID, "parked": true, "until": until, "reason": reason,
	})
}

// unpark lifts a park (resume_task on a parked task); a hold it released is
// announced claimable.
func (h *Handlers) unpark(project, agent string, task *models.Task) (*mcp.CallToolResult, error) {
	released, err := h.db.UnparkTask(project, task.ID, agent)
	if err != nil {
		return taskOpError(err, "failed to unpark task: %v", err), nil
	}
	if released {
		h.announceRelease(project, task.ID)
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "unparked", Agent: agent, Project: project, Label: task.Title})
	return h.resultJSONTracked(project, agent, "resume_task", map[string]any{
		"task_id": task.ID, "parked": false, "released": released,
	})
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
	if !h.callerMayReassign(project, agent, task) {
		return nil, permissionError(CodeForbidden, fmt.Sprintf(
			"only the dispatcher (%s), an executive or the doer's lead may %s task %s", task.DispatchedBy, verb, taskID))
	}
	return task, nil
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
