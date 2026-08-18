package relay

import (
	"agent-relay/internal/db"
	"agent-relay/internal/models"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// typedTaskError renders a *db.TaskError as a structured tool error whose body is
// {"error": <code>, "message": <msg>} so a caller can branch on a stable code
// (park on TASK_LEASE_HELD, treat TASK_STATE_CONFLICT as a lost race) instead of
// pattern-matching a prose string — the "no infinite-retry path" contract.
func typedTaskError(te *db.TaskError) *mcp.CallToolResult {
	b, err := json.Marshal(map[string]string{"error": te.Code, "message": te.Msg})
	if err != nil {
		return mcp.NewToolResultError(te.Error())
	}
	return mcp.NewToolResultError(string(b))
}

// missingTicketFields returns which of goal / acceptance_criteria / dod are
// absent, in dispatch order. acceptanceCriteria is the raw wire value (a JSON
// array string); it counts as present only when it parses to a non-empty list
// with at least one non-blank item. Empty return = a complete typed ticket.
func missingTicketFields(goal, acceptanceCriteria, dod string) []string {
	var missing []string
	if strings.TrimSpace(goal) == "" {
		missing = append(missing, "goal")
	}
	if !hasAcceptanceItems(acceptanceCriteria) {
		missing = append(missing, "acceptance_criteria")
	}
	if strings.TrimSpace(dod) == "" {
		missing = append(missing, "dod")
	}
	return missing
}

// hasAcceptanceItems is true when raw is a JSON array carrying ≥1 non-blank
// string — the "list of individually testable items" the review gate verdicts
// against. A bare string, empty array, or unparseable value is not enough.
func hasAcceptanceItems(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var items []string
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return false
	}
	for _, it := range items {
		if strings.TrimSpace(it) != "" {
			return true
		}
	}
	return false
}

// ticketRefusal formats the per-field refusal shown to a dispatcher when a
// typed-ticket project is missing fields.
func ticketRefusal(project string, missing []string) string {
	// The missing list is bracketed so callers (and tests) can read exactly
	// which fields were absent without tripping on the guidance sentence, which
	// necessarily names all three field keys.
	return fmt.Sprintf("typed ticket required for project '%s': missing [%s]. "+
		"Dispatch with goal (intent), acceptance_criteria (JSON array of individually testable items) and dod (definition of done).",
		project, strings.Join(missing, ", "))
}

func (h *Handlers) HandleDispatchTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	profile := req.GetString("profile", "")
	requiredSkill := req.GetString("required_skill", "")
	// Quota check: tasks
	if qErr := h.db.CheckQuotaError(project, agent, "tasks"); qErr != "" {
		return mcp.NewToolResultError(qErr), nil
	}

	// Auto-resolve profile from skill if not specified
	if profile == "" && requiredSkill != "" {
		best, _ := h.db.FindBestProfileForSkill(project, requiredSkill)
		if best != nil {
			profile = best.Slug
		}
	}
	if profile == "" {
		return mcp.NewToolResultError("profile is required (or provide required_skill)"), nil
	}
	title := req.GetString("title", "")
	if title == "" {
		return mcp.NewToolResultError("title is required"), nil
	}
	description := req.GetString("description", "")
	priority := req.GetString("priority", "P2")
	parentTaskID := optionalString(req.GetString("parent_task_id", ""))
	boardID := optionalString(req.GetString("board_id", ""))

	// Typed ticket (V-lifecycle). Refuse an incomplete ticket only where the
	// project opts in — free-form projects dispatch unchanged.
	goal := req.GetString("goal", "")
	acceptanceCriteria := req.GetString("acceptance_criteria", "")
	dod := req.GetString("dod", "")
	if h.db.ProjectRequiresTypedTicket(project) {
		if missing := missingTicketFields(goal, acceptanceCriteria, dod); len(missing) > 0 {
			return mcp.NewToolResultError(ticketRefusal(project, missing)), nil
		}
	}
	ticket := db.TypedTicket{Goal: goal, AcceptanceCriteria: acceptanceCriteria, Dod: dod}

	// Resolve truncated board_id prefix to full UUID
	if boardID != nil && len(*boardID) < 36 {
		boards, _ := h.db.ListBoards(project)
		for _, b := range boards {
			if strings.HasPrefix(b.ID, *boardID) {
				boardID = &b.ID
				break
			}
		}
	}

	// Auto-create "human" profile if dispatching to it for the first time
	if profile == "human" {
		existing, _ := h.db.GetProfile(project, "human")
		if existing == nil {
			_, _ = h.db.RegisterProfile(project, "human", "Human Operator",
				"Tasks that require human action (API keys, approvals, purchases, manual config)",
				"[]")
		}
	}

	// Auto-create a default "backlog" board if none specified and none exist
	var autoBoard *models.Board
	if boardID == nil {
		boards, _ := h.db.ListBoards(project)
		if len(boards) == 0 {
			autoBoard, _ = h.db.CreateBoard(project, "Backlog", "backlog", "Auto-created default board", agent)
			if autoBoard != nil {
				boardID = &autoBoard.ID
			}
		} else {
			// Use the first existing board as default
			boardID = &boards[0].ID
		}
	}

	task, err := h.db.DispatchTask(project, profile, agent, title, description, priority, parentTaskID, boardID, ticket)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to dispatch task: %v", err)), nil
	}

	// Push notification for P0/P1 tasks
	if priority == "P0" || priority == "P1" {
		h.registry.NotifyProfile(project, profile, agent, fmt.Sprintf("[%s] %s", priority, title), task.ID)
	}

	// Auto-notification: send inbox message to agents running this profile
	agents, _ := h.db.GetAgentsByProfile(project, profile)
	for _, a := range agents {
		if a.Name == agent {
			continue // don't notify the dispatcher
		}
		subject := fmt.Sprintf("New task: %s", title)
		content := fmt.Sprintf("[%s] %s\n\nTask ID: %s\nProfile: %s\nDispatched by: %s", priority, title, task.ID, profile, agent)
		if description != "" && len(description) <= 200 {
			content += "\n\n" + description
		}
		taskID := task.ID
		_, _ = h.db.InsertMessage(project, agent, a.Name, "task", subject, content, fmt.Sprintf(`{"task_id":"%s"}`, taskID), "P2", 14400, nil, nil)
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "dispatch", Agent: agent, Project: project, Target: profile, Label: title})
	emitTaskEvent(h.events, "task.dispatched", "dispatch", project, task)

	resp := map[string]any{"task": task}
	if autoBoard != nil {
		resp["auto_board"] = autoBoard
		resp["hint"] = fmt.Sprintf("Auto-created 'backlog' board (id: %s) since no boards existed.", autoBoard.ID)
	}

	// Dedup warning: check for similar active tasks on same profile
	similar, _ := h.db.FindSimilarTasks(project, profile, title)
	if len(similar) > 0 {
		// Filter out the task we just created
		var dupes []map[string]string
		for _, s := range similar {
			if s.ID != task.ID {
				dupes = append(dupes, map[string]string{"id": s.ID, "title": s.Title, "status": s.Status})
			}
		}
		if len(dupes) > 0 {
			resp["warning"] = fmt.Sprintf("Found %d similar active task(s) on profile '%s'", len(dupes), profile)
			resp["similar"] = dupes
		}
	}

	return h.resultJSONTracked(project, agent, "dispatch_task", resp)
}

func (h *Handlers) HandleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	task, err := h.db.ClaimTask(taskID, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to claim task: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "claim", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.claimed", "claim", project, task)
	pushStatusAsync(h.getConnector(), task, "accepted", nil)
	return h.resultJSONTracked(project, agent, "claim_task", task)
}

// HandleReclaimTask takes over a DEAD holder's task — the primitive niwa's
// resume-protocol part 3 (supervisor-driven re-claim + ack) stands on. It
// refuses a live holder's task with TASK_LEASE_HELD and a lost CAS race with
// TASK_STATE_CONFLICT, both as typed structured errors so the caller parks
// rather than hot-loops. On success the task is 'accepted' under the caller with
// a fresh lease, and a task.lease_transferred event carries {from,to,reason}.
func (h *Handlers) HandleReclaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	task, err := h.db.ReclaimTask(taskID, agent, project)
	if err != nil {
		var te *db.TaskError
		if errors.As(err, &te) {
			return typedTaskError(te), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("failed to reclaim task: %v", err)), nil
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "claim", Agent: agent, Project: project, Label: task.Title})
	// The lease holder changed — announce the transfer (SSE + audit already
	// written by the DB layer) so a subscriber sees the hand-off and its reason
	// before the ordinary claimed event.
	if task.LeaseTransfer != nil {
		emitTaskEvent(h.events, "task.lease_transferred", "claim", project, task, map[string]any{
			"from":   task.LeaseTransfer.From,
			"to":     task.LeaseTransfer.To,
			"reason": task.LeaseTransfer.Reason,
		})
	}
	emitTaskEvent(h.events, "task.claimed", "claim", project, task)
	pushStatusAsync(h.getConnector(), task, "accepted", nil)
	return h.resultJSONTracked(project, agent, "reclaim_task", task)
}

func (h *Handlers) HandleStartTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	task, err := h.db.StartTask(taskID, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to start task: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "start", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.in_progress", "start", project, task)
	pushStatusAsync(h.getConnector(), task, "in-progress", nil)
	return h.resultJSONTracked(project, agent, "start_task", task)
}

// HandleResumeTask transitions a blocked task back to in-progress.
// Thin wrapper over StartTask (the DB allows the blocked→in-progress transition
// already) — kept as a distinct MCP tool so agents discovering tools don't have
// to guess that start_task resumes too.
func (h *Handlers) HandleResumeTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	existing, err := h.db.GetTask(taskID, project)
	if err != nil || existing == nil {
		return mcp.NewToolResultError("task not found"), nil
	}
	if existing.Status != "blocked" {
		return mcp.NewToolResultError(fmt.Sprintf("task is not blocked (status=%s)", existing.Status)), nil
	}

	task, err := h.db.StartTask(taskID, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to resume task: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "resume", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.in_progress", "resume", project, task)
	pushStatusAsync(h.getConnector(), task, "in-progress", nil)

	return h.resultJSONTracked(project, agent, "resume_task", task)
}

func (h *Handlers) HandleReviewTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Git zone: where the work lives, for the external review gate. Written
	// BEFORE the transition so the re-read task (and its in_review event)
	// carries the fields.
	gitBranch := optionalString(req.GetString("git_branch", ""))
	gitWorktree := optionalString(req.GetString("git_worktree", ""))
	gitTarget := optionalString(req.GetString("git_target", ""))
	if gitBranch != nil || gitWorktree != nil || gitTarget != nil {
		if err := h.db.SetTaskGit(taskID, project, gitBranch, gitWorktree, gitTarget); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to record git fields: %v", err)), nil
		}
	}

	task, err := h.db.ReviewTask(taskID, agent, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to mark task in-review: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "review", Agent: agent, Project: project, Target: task.DispatchedBy, Label: task.Title})
	// The in_review event carries the git zone + the submitting agent, so a
	// gate subscribed to the stream can act without a follow-up GET.
	gitPayload := map[string]any{"doer": agent}
	if task.GitBranch != nil {
		gitPayload["git_branch"] = *task.GitBranch
	}
	if task.GitWorktree != nil {
		gitPayload["git_worktree"] = *task.GitWorktree
	}
	if task.GitTarget != nil {
		gitPayload["git_target"] = *task.GitTarget
	}
	emitTaskEvent(h.events, "task.in_review", "review", project, task, gitPayload)

	// Notify dispatcher — work is up for review.
	h.registry.Notify(project, task.DispatchedBy, agent, fmt.Sprintf("In review: %s", task.Title), task.ID)

	// Write-back (Linear mode): after the local stamp succeeds, move the issue to
	// In Review + optional comment, fire-and-forget. No-op in native.
	comment := optionalString(req.GetString("comment", ""))
	pushStatusAsync(h.getConnector(), task, "in-review", comment)

	return h.resultJSONTracked(project, agent, "review_task", task)
}

// HandleComment posts a comment on a task. On a Linear-mirrored task it goes to
// the Linear issue (Linear is SSOT); otherwise it is saved as a local progress
// note so the action still lands somewhere.
func (h *Handlers) HandleComment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	body := strings.TrimSpace(req.GetString("body", ""))
	if taskID == "" || body == "" {
		return mcp.NewToolResultError("task_id and body are required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	task, err := h.db.GetTask(taskID, project)
	if err != nil || task == nil {
		return mcp.NewToolResultError("task not found"), nil
	}

	conn := h.getConnector()
	if task.Source == "linear" && conn.Active() && task.LinearIssueID != nil && *task.LinearIssueID != "" {
		if err := conn.Comment(*task.LinearIssueID, body); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to post comment to Linear: %v", err)), nil
		}
		return h.resultJSONTracked(project, agent, "comment", map[string]any{"posted": "linear", "task_id": taskID})
	}
	if err := h.db.AddProgressNote(taskID, project, agent, body); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to add note: %v", err)), nil
	}
	return h.resultJSONTracked(project, agent, "comment", map[string]any{"posted": "note", "task_id": taskID})
}

func (h *Handlers) HandleCompleteTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result := optionalString(req.GetString("result", ""))

	task, err := h.db.CompleteTask(taskID, agent, project, result)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to complete task: %v", err)), nil
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "complete", Agent: agent, Project: project, Target: task.DispatchedBy, Label: task.Title})
	emitTaskEvent(h.events, "task.done", "complete", project, task)
	pushStatusAsync(h.getConnector(), task, "done", result)

	// Notify dispatcher
	h.registry.Notify(project, task.DispatchedBy, agent, fmt.Sprintf("Task done: %s", task.Title), task.ID)

	// If this task has a parent, check if all sibling subtasks are now complete
	if task.ParentTaskID != nil {
		allDone, total, doneCount := h.db.CheckSubtasksComplete(*task.ParentTaskID, project)
		if allDone {
			parent, _ := h.db.GetTask(*task.ParentTaskID, project)
			if parent != nil {
				h.registry.Notify(project, parent.DispatchedBy, agent,
					fmt.Sprintf("All %d subtasks complete for: %s", total, parent.Title), parent.ID)
				// Also notify the assigned agent on the parent task
				if parent.AssignedTo != nil && *parent.AssignedTo != parent.DispatchedBy {
					h.registry.Notify(project, *parent.AssignedTo, agent,
						fmt.Sprintf("All %d subtasks complete for your task: %s", total, parent.Title), parent.ID)
				}
			}
		} else {
			// Partial progress notification to parent dispatcher
			parent, _ := h.db.GetTask(*task.ParentTaskID, project)
			if parent != nil {
				h.registry.Notify(project, parent.DispatchedBy, agent,
					fmt.Sprintf("Subtask done (%d/%d): %s → %s", doneCount, total, task.Title, parent.Title), parent.ID)
			}
		}
	}

	return h.resultJSONTracked(project, agent, "complete_task", task)
}

func (h *Handlers) HandleBlockTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reason := optionalString(req.GetString("reason", ""))

	task, err := h.db.BlockTask(taskID, agent, project, reason)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to block task: %v", err)), nil
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "block", Agent: agent, Project: project, Target: task.DispatchedBy, Label: task.Title})
	blockedExtra := map[string]any{}
	if reason != nil {
		blockedExtra["reason"] = *reason
	}
	emitTaskEvent(h.events, "task.blocked", "block", project, task, blockedExtra)
	pushStatusAsync(h.getConnector(), task, "blocked", reason)

	// Notify dispatcher — blocked is critical
	reasonStr := ""
	if reason != nil {
		reasonStr = ": " + *reason
	}
	h.registry.Notify(project, task.DispatchedBy, agent, fmt.Sprintf("BLOCKED: %s%s", task.Title, reasonStr), task.ID)

	// Phase 4: Bubble notification up parent chain
	if task.ParentTaskID != nil {
		parentChain, _ := h.db.GetParentChain(taskID, project)
		for _, parent := range parentChain {
			h.registry.Notify(project, parent.DispatchedBy, agent,
				fmt.Sprintf("Subtask blocked: '%s' → %s%s", task.Title, parent.Title, reasonStr), task.ID)
		}
	}

	return h.resultJSONTracked(project, agent, "block_task", task)
}

func (h *Handlers) HandleCancelTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reason := optionalString(req.GetString("reason", ""))

	task, err := h.db.CancelTask(taskID, agent, project, reason)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to cancel task: %v", err)), nil
	}
	pushStatusAsync(h.getConnector(), task, "cancelled", reason)

	// Notify dispatcher
	reasonStr := ""
	if reason != nil {
		reasonStr = ": " + *reason
	}
	h.registry.Notify(project, task.DispatchedBy, agent, fmt.Sprintf("Task cancelled: %s%s", task.Title, reasonStr), task.ID)

	// Notify assigned agent (if different from canceller and dispatcher)
	if task.AssignedTo != nil && *task.AssignedTo != agent && *task.AssignedTo != task.DispatchedBy {
		h.registry.Notify(project, *task.AssignedTo, agent, fmt.Sprintf("Your task was cancelled: %s%s", task.Title, reasonStr), task.ID)
	}

	// If this task has a parent, check if all sibling subtasks are now complete (cancelled counts)
	if task.ParentTaskID != nil {
		allDone, total, doneCount := h.db.CheckSubtasksComplete(*task.ParentTaskID, project)
		if allDone {
			parent, _ := h.db.GetTask(*task.ParentTaskID, project)
			if parent != nil {
				h.registry.Notify(project, parent.DispatchedBy, agent,
					fmt.Sprintf("All %d subtasks resolved for: %s", total, parent.Title), parent.ID)
			}
		} else {
			parent, _ := h.db.GetTask(*task.ParentTaskID, project)
			if parent != nil {
				h.registry.Notify(project, parent.DispatchedBy, agent,
					fmt.Sprintf("Subtask cancelled (%d/%d resolved): %s → %s", doneCount, total, task.Title, parent.Title), parent.ID)
			}
		}
	}

	return h.resultJSONTracked(project, agent, "cancel_task", task)
}

func (h *Handlers) HandleUpdateTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	title := optionalString(req.GetString("title", ""))
	description := optionalString(req.GetString("description", ""))
	priority := optionalString(req.GetString("priority", ""))
	boardID := optionalString(req.GetString("board_id", ""))
	progressNote := req.GetString("progress_note", "")

	task, err := h.db.UpdateTaskFields(taskID, project, title, description, priority, boardID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to update task: %v", err)), nil
	}

	if progressNote != "" {
		if err := h.db.AddProgressNote(taskID, project, agent, progressNote); err == nil {
			h.events.Emit(MCPEvent{Type: "task", Action: "progress", Agent: agent, Project: project, Label: task.Title})
		}
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "update", Agent: agent, Project: project, Label: task.Title})
	return h.resultJSONTracked(project, agent, "update_task", task)
}

func (h *Handlers) HandleArchiveTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	status := req.GetString("status", "")
	boardID := req.GetString("board_id", "")

	count, err := h.db.ArchiveTasks(project, status, boardID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to archive tasks: %v", err)), nil
	}

	msg := fmt.Sprintf("Archived %d tasks", count)
	if status != "" {
		msg += fmt.Sprintf(" (status=%s)", status)
	}
	if boardID != "" {
		msg += fmt.Sprintf(" (board=%s)", boardID)
	}
	return mcp.NewToolResultText(msg), nil
}

func (h *Handlers) HandleMoveTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	boardID := optionalString(req.GetString("board_id", ""))

	if boardID == nil {
		return mcp.NewToolResultError("board_id is required"), nil
	}

	// Resolve truncated board_id prefix
	if len(*boardID) > 0 && len(*boardID) < 36 {
		boards, _ := h.db.ListBoards(project)
		for _, b := range boards {
			if strings.HasPrefix(b.ID, *boardID) {
				boardID = &b.ID
				break
			}
		}
	}

	task, err := h.db.UpdateTaskFields(taskID, project, nil, nil, nil, boardID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to move task: %v", err)), nil
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "move", Agent: agent, Project: project, Label: task.Title})
	return h.resultJSONTracked(project, agent, "move_task", task)
}

func (h *Handlers) HandleBatchCompleteTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	tasksJSON := req.GetString("tasks", "")

	var items []struct {
		TaskID string  `json:"task_id"`
		Result *string `json:"result"`
	}
	// Accept the common mistake task_ids:["..."] as a shorthand for
	// tasks:[{task_id:"..."}] (no result).
	if tasksJSON == "" {
		if idsJSON := req.GetString("task_ids", ""); idsJSON != "" {
			var ids []string
			if err := json.Unmarshal([]byte(idsJSON), &ids); err == nil {
				for _, id := range ids {
					items = append(items, struct {
						TaskID string  `json:"task_id"`
						Result *string `json:"result"`
					}{TaskID: id})
				}
			}
		}
	} else {
		if err := json.Unmarshal([]byte(tasksJSON), &items); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid tasks JSON: %v", err)), nil
		}
	}
	if len(items) == 0 {
		return mcp.NewToolResultError("tasks is required — pass tasks:'[{\"task_id\":\"...\",\"result\":\"...\"}]' (JSON string). As a shortcut, task_ids:'[\"id1\",\"id2\"]' is also accepted."), nil
	}

	var completed []string
	var errors []string
	for _, item := range items {
		taskID, err := h.resolveTaskID(item.TaskID, project)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", item.TaskID, err))
			continue
		}
		task, err := h.db.CompleteTask(taskID, agent, project, item.Result)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", taskID, err))
			continue
		}
		completed = append(completed, taskID)
		h.events.Emit(MCPEvent{Type: "task", Action: "complete", Agent: agent, Project: project, Label: task.Title})
	}

	return h.resultJSONTracked(project, agent, "batch_complete_tasks", map[string]any{
		"completed": completed,
		"errors":    errors,
		"total":     len(items),
	})
}

func (h *Handlers) HandleBatchDispatchTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	tasksJSON := req.GetString("tasks", "[]")

	var items []struct {
		Profile            string   `json:"profile"`
		Title              string   `json:"title"`
		Description        string   `json:"description"`
		Priority           string   `json:"priority"`
		BoardID            *string  `json:"board_id"`
		Goal               string   `json:"goal"`
		AcceptanceCriteria []string `json:"acceptance_criteria"`
		Dod                string   `json:"dod"`
	}
	if err := json.Unmarshal([]byte(tasksJSON), &items); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid tasks JSON: %v", err)), nil
	}
	if len(items) == 0 {
		return mcp.NewToolResultError("tasks is required — pass tasks:'[{\"profile\":\"...\",\"title\":\"...\",\"priority\":\"P2\",\"board_id\":\"...\"}]' (JSON string). Only profile and title are required per item."), nil
	}

	// Same per-project typed-ticket enforcement as dispatch_task; a failing item
	// lands in errors and the batch continues (per-item, not all-or-nothing).
	requireTicket := h.db.ProjectRequiresTypedTicket(project)

	var dispatched []map[string]string
	var errors []string
	for _, item := range items {
		if item.Profile == "" || item.Title == "" {
			errors = append(errors, fmt.Sprintf("missing profile or title: %+v", item))
			continue
		}
		// acceptance_criteria arrives as a real JSON array per item; store it as
		// the same JSON-array string the single dispatch path persists.
		acJSON := "[]"
		if len(item.AcceptanceCriteria) > 0 {
			if b, err := json.Marshal(item.AcceptanceCriteria); err == nil {
				acJSON = string(b)
			}
		}
		if requireTicket {
			if missing := missingTicketFields(item.Goal, acJSON, item.Dod); len(missing) > 0 {
				errors = append(errors, fmt.Sprintf("%s: %s", item.Title, ticketRefusal(project, missing)))
				continue
			}
		}
		priority := item.Priority
		if priority == "" {
			priority = "P2"
		}
		ticket := db.TypedTicket{Goal: item.Goal, AcceptanceCriteria: acJSON, Dod: item.Dod}
		task, err := h.db.DispatchTask(project, item.Profile, agent, item.Title, item.Description, priority, nil, item.BoardID, ticket)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", item.Title, err))
			continue
		}
		dispatched = append(dispatched, map[string]string{"id": task.ID, "title": task.Title})
		h.events.Emit(MCPEvent{Type: "task", Action: "dispatch", Agent: agent, Project: project, Target: item.Profile, Label: item.Title})
		emitTaskEvent(h.events, "task.dispatched", "dispatch", project, task)
	}

	return h.resultJSONTracked(project, agent, "batch_dispatch_tasks", map[string]any{
		"dispatched": dispatched,
		"errors":     errors,
		"total":      len(items),
	})
}

func (h *Handlers) HandleGetTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	taskID, rErr := h.resolveTaskID(taskID, project)
	if rErr != nil {
		return mcp.NewToolResultError(rErr.Error()), nil
	}
	includeSubtasks := req.GetBool("include_subtasks", false)

	var task *models.Task
	var err error
	if includeSubtasks {
		task, err = h.db.GetTaskWithSubtasks(taskID, project)
	} else {
		task, err = h.db.GetTask(taskID, project)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get task: %v", err)), nil
	}
	if task == nil {
		return mcp.NewToolResultError("task not found"), nil
	}

	return h.resultJSONTracked(project, "", "get_task", task)
}

func (h *Handlers) HandleListTasks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	status := req.GetString("status", "")
	profile := req.GetString("profile", "")
	priority := req.GetString("priority", "")
	assignedTo := req.GetString("assigned_to", "")
	boardID := req.GetString("board_id", "")
	limit := clampLimit(req.GetInt("limit", 50))
	includeArchived := req.GetBool("include_archived", false)

	tasks, err := h.db.ListTasks(project, status, profile, priority, assignedTo, boardID, limit, includeArchived)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list tasks: %v", err)), nil
	}
	if tasks == nil {
		tasks = []models.Task{}
	}

	// Truncate descriptions to save tokens in list view (use get_task for full details)
	for i := range tasks {
		if len(tasks[i].Description) > 200 {
			tasks[i].Description = tasks[i].Description[:200] + "…"
		}
		if tasks[i].Result != nil && len(*tasks[i].Result) > 200 {
			truncated := (*tasks[i].Result)[:200] + "…"
			tasks[i].Result = &truncated
		}
	}

	if f := req.GetString("format", "md"); f == "md" || f == "table" {
		rows := make([][]string, len(tasks))
		for i, t := range tasks {
			outcome := strOrDash(t.Result)
			if t.Status == "blocked" {
				outcome = "BLOCKED: " + strOrDash(t.BlockedReason)
			}
			rows[i] = []string{
				t.ID, t.Status, t.Priority, t.ProfileSlug, strOrDash(t.AssignedTo),
				t.Title, t.Description, outcome,
			}
		}
		table := renderTable([]string{"id", "status", "priority", "profile", "assigned_to", "title", "description", "result_or_blocked_reason"}, rows)
		return h.resultTextTracked(project, "", "list_tasks", fmt.Sprintf("%d tasks\n%s", len(tasks), table))
	}

	return h.resultJSONTracked(project, "", "list_tasks", map[string]any{
		"count": len(tasks),
		"tasks": tasks,
	})
}
