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
	category, retryable := taskErrorCategory(te.Code)
	// Keep the legacy "error" alias (= code) alongside the canonical envelope so
	// any caller keying on "error" still works; new callers read code/category.
	return toolError(te.Code, category, retryable, te.Msg, map[string]any{"error": te.Code})
}

// taskOpError routes the error from a task state-machine DB op. A typed
// *db.TaskError (lost CAS race → TASK_STATE_CONFLICT, live lease → TASK_LEASE_HELD,
// missing → TASK_NOT_FOUND) becomes the typed envelope with the CORRECT category —
// crucially a lost race is validation/non-retryable, so a double-claim loser PARKS
// instead of hot-looping. Anything else is an unclassified internal failure. Use
// this (never a bare fmt.Sprintf wrap) on every claim/start/review/complete/block/
// cancel/resume/update path, or a conflict silently reads as retryable.
func taskOpError(err error, format string, a ...any) *mcp.CallToolResult {
	var te *db.TaskError
	if errors.As(err, &te) {
		return typedTaskError(te)
	}
	return toolResultError(fmt.Sprintf(format, a...))
}

// taskErrorCategory maps a task-state code to the uniform taxonomy. EVERY task
// conflict is isRetryable=FALSE — the contract is "PARK, don't hot-loop":
//   - TASK_LEASE_HELD: a LIVE holder owns the task. The lease is temporally
//     transient (it will lapse), but hot-looping the same reclaim now only
//     spins against a live holder — the caller must park and let the supervisor
//     path or lease-expiry resolve it, so isRetryable=false despite the
//     transient category.
//   - TASK_STATE_CONFLICT / TASK_NOT_FOUND: the state moved or never existed;
//     the same call as-is keeps failing — re-fetch first. Validation.
func taskErrorCategory(code string) (category string, retryable bool) {
	switch code {
	case db.CodeTaskLeaseHeld:
		return CategoryTransient, false
	case db.CodeTaskStateConflict, db.CodeTaskNotFound,
		db.CodeRunStateInvalid, db.CodeRunContainer, db.CodeRunStateConflict:
		return CategoryValidation, false
	default:
		return CategoryValidation, false
	}
}

// Typed-ticket enforcement (missing-field detection, the per-project refusal, and
// the refusal message) lives in the db package on TypedTicket — see
// db.TypedTicket.Missing and db.TypedTicketError. It is enforced at the single
// creation choke (db.DispatchTask) so no dispatch path can drift or bypass it.
// Handlers below only translate a *db.TypedTicketError into their response shape.

func (h *Handlers) HandleDispatchTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	profile := req.GetString("profile", "")
	requiredSkill := req.GetString("required_skill", "")
	// Quota check: tasks
	if qErr := h.db.CheckQuotaError(project, agent, "tasks"); qErr != "" {
		return toolResultError(qErr), nil
	}

	// Auto-resolve profile from skill if not specified
	if profile == "" && requiredSkill != "" {
		best, _ := h.db.FindBestProfileForSkill(project, requiredSkill)
		if best != nil {
			profile = best.Slug
		}
	}
	if profile == "" {
		return toolResultError("profile is required (or provide required_skill)"), nil
	}
	title := req.GetString("title", "")
	if title == "" {
		return toolResultError("title is required"), nil
	}
	description := req.GetString("description", "")

	// GetString silently falls back to the default when the arg is present but
	// the wrong JSON type (e.g. priority sent as the integer 1) — a task landed
	// P2 without anyone asking for it (repro: task 7670216a). Check the raw
	// argument so a wrong-typed or invalid priority is refused, never coerced.
	priority := "P2"
	if raw, given := req.GetArguments()["priority"]; given {
		str, isStr := raw.(string)
		if !isStr || !isValidPriority(str) {
			return validationError(CodeInvalidArgument, fmt.Sprintf(
				"priority must be a string, one of P0, P1, P2, P3 (got %#v)", raw)), nil
		}
		priority = str
	}

	parentTaskID := optionalString(req.GetString("parent_task_id", ""))
	boardID := optionalString(req.GetString("board_id", ""))

	// Correlation (trace_id v1): an explicit trace_id must be well-formed (32
	// lowercase hex) — refused, never silently accepted-as-garbage or dropped.
	// Omitted is the normal case: DispatchTask mints or inherits one.
	traceID := optionalString(req.GetString("trace_id", ""))
	if traceID != nil && !db.ValidTraceID(*traceID) {
		return validationError(CodeInvalidArgument, "trace_id must be 32 lowercase hex characters"), nil
	}

	// Typed ticket (V-lifecycle). Enforcement is the single guard in
	// db.DispatchTask (fires below): an incomplete ticket on an enforced project
	// is refused as a *db.TypedTicketError; free-form projects dispatch unchanged.
	ticket := db.TypedTicket{
		Goal:               req.GetString("goal", ""),
		AcceptanceCriteria: req.GetString("acceptance_criteria", ""),
		Dod:                req.GetString("dod", ""),
	}

	// Resolve a truncated board_id UUID prefix, or a board slug, to the full UUID
	// — so a caller can copy either column straight out of a BoardRequiredError
	// refusal (which lists both) without a separate lookup round-trip.
	if boardID != nil && len(*boardID) < 36 {
		boards, _ := h.db.ListBoards(project)
		for _, b := range boards {
			if strings.HasPrefix(b.ID, *boardID) || b.Slug == *boardID {
				boardID = &b.ID
				break
			}
		}
	}

	backlog := req.GetBool("backlog", false)
	task, autoBoard, err := h.dispatchCore(project, agent, profile, title, description, priority, parentTaskID, boardID, ticket, backlog, traceID)
	if err != nil {
		var tte *db.TypedTicketError
		if errors.As(err, &tte) {
			return toolResultError(tte.Error()), nil
		}
		var ite *db.InvalidTitleError
		if errors.As(err, &ite) {
			return validationError(CodeInvalidArgument, ite.Error()), nil
		}
		var bre *db.BoardRequiredError
		if errors.As(err, &bre) {
			return validationError(CodeInvalidArgument, bre.Error()), nil
		}
		return toolResultError(fmt.Sprintf("failed to dispatch task: %v", err)), nil
	}

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

// dispatchCore is the shared task-creation pipeline behind both dispatch_task
// (MCP) and the inbound-signal webhook (steal #9): it auto-creates the 'human'
// profile and a default 'backlog' board when needed, creates the task, pushes a
// P0/P1 notification, delivers an inbox message to every agent running the
// profile (a durable 'queued' delivery so an idle lane's wake-poll sees it), and
// emits the task.dispatched event that drives the normal dispatch pipeline.
// Callers own quota/ticket-validation/dedup-warning; this is the create+announce
// core so a signal-created task is indistinguishable from a hand-dispatched one.
func (h *Handlers) dispatchCore(project, dispatchedBy, profile, title, description, priority string, parentTaskID, boardID *string, ticket db.TypedTicket, backlog bool, traceID *string) (*models.Task, *models.Board, error) {
	// Typed-ticket guard, hoisted AHEAD of the profile/board auto-create below.
	// db.DispatchTask is the authoritative choke and would refuse a bare ticket on
	// an enforced project regardless — but by then this function may already have
	// auto-created a stray empty "Backlog" board / "human" profile for a dispatch
	// that never lands (a bare signal-webhook or cron ticket on niwa). Fail fast on
	// the same predicate (ticket.Missing) so a refused dispatch leaves no residue.
	if h.db.ProjectRequiresTypedTicket(project) {
		if missing := ticket.Missing(); len(missing) > 0 {
			return nil, nil, &db.TypedTicketError{Project: project, Missing: missing}
		}
		if reason := db.InvalidTitleReason(title); reason != "" {
			return nil, nil, &db.InvalidTitleError{Project: project, Title: title, Reason: reason}
		}
	}

	// Auto-create "human" profile if dispatching to it for the first time.
	if profile == "human" {
		existing, _ := h.db.GetProfile(project, "human")
		if existing == nil {
			_, _ = h.db.RegisterProfile(project, "human", "Human Operator",
				"Tasks that require human action (API keys, approvals, purchases, manual config)",
				"[]")
		}
	}

	// Auto-create a default "backlog" board only when the project has none yet.
	// One or more existing boards with board_id omitted is now db.DispatchTask's
	// call: the sole board unambiguously, or a *db.BoardRequiredError refusal
	// naming every board when more than one exists — never a silent first-board
	// pick (that was the bug: tasks mis-filed onto the oldest board unnoticed).
	var autoBoard *models.Board
	if boardID == nil {
		boards, _ := h.db.ListBoards(project)
		if len(boards) == 0 {
			autoBoard, _ = h.db.CreateBoard(project, "Backlog", "backlog", "Auto-created default board", dispatchedBy)
			if autoBoard != nil {
				boardID = &autoBoard.ID
			}
		}
	}

	task, err := h.db.DispatchTask(project, profile, dispatchedBy, title, description, priority, parentTaskID, boardID, ticket, backlog, traceID)
	if err != nil {
		return nil, nil, err
	}

	// A backlog task is groomed-but-not-claimable: emit only the visual event so
	// the board shows it, and SKIP every claim-signal (P0/P1 push, the per-agent
	// inbox delivery, and the task.dispatched event that drives auto-claim). It
	// becomes claimable + surfaced only when promote_task lifts it to pending.
	if backlog {
		h.events.Emit(MCPEvent{Type: "task", Action: "backlog", Agent: dispatchedBy, Project: project, Target: profile, Label: title})
		return task, autoBoard, nil
	}

	h.announceClaimable(project, dispatchedBy, profile, title, description, priority, task)
	return task, autoBoard, nil
}

// announceClaimable fires the claim signals for a now-claimable task: the P0/P1
// push, the durable per-agent inbox delivery (niwa's idle-wake poll counts queued
// deliveries), and the task.dispatched event that drives the normal auto-claim
// pipeline. Shared by dispatchCore (a pending dispatch) and promote_task (a
// backlog task lifted to pending) so a promoted task is surfaced exactly like a
// freshly-dispatched one.
func (h *Handlers) announceClaimable(project, dispatchedBy, profile, title, description, priority string, task *models.Task) {
	if priority == "P0" || priority == "P1" {
		h.registry.NotifyProfile(project, profile, dispatchedBy, fmt.Sprintf("[%s] %s", priority, title), task.ID)
	}
	agents, _ := h.db.GetAgentsByProfile(project, profile)
	for _, a := range agents {
		if a.Name == dispatchedBy {
			continue // don't notify the dispatcher
		}
		subject := fmt.Sprintf("New task: %s", title)
		content := fmt.Sprintf("[%s] %s\n\nTask ID: %s\nProfile: %s\nDispatched by: %s", priority, title, task.ID, profile, dispatchedBy)
		if description != "" && len(description) <= 200 {
			content += "\n\n" + description
		}
		_, _ = h.db.InsertMessageWithDeliveries(project, dispatchedBy, a.Name, "task", subject, content, fmt.Sprintf(`{"task_id":"%s"}`, task.ID), "P2", 14400, nil, nil, []string{a.Name}, "")
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "dispatch", Agent: dispatchedBy, Project: project, Target: profile, Label: title})
	emitTaskEvent(h.events, "task.dispatched", "dispatch", project, task)
}

func (h *Handlers) HandleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	task, err := h.db.ClaimTask(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to claim task: %v", err), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "claim", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.claimed", "claim", project, task)
	pushStatusAsync(h.getConnector(), task, "accepted", nil)
	return h.resultJSONTracked(project, agent, "claim_task", task)
}

// HandlePromoteTask lifts a groomed 'backlog' task to 'pending' (claimable) and
// announces it exactly like a fresh dispatch, so an agent picks it up only once a
// human/lead has promoted it. Lifecycle-enforced by db.PromoteTask (only
// backlog→pending); any other origin returns an invalid-transition error.
func (h *Handlers) HandlePromoteTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	task, changed, err := h.db.PromoteTask(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to promote task: %v", err), nil
	}
	// Announce only on a real backlog→pending promotion. A no-op promote of an
	// already-pending task must NOT re-fire the fleet wake / task.dispatched / P0
	// push (idempotency).
	if changed {
		h.announceClaimable(project, agent, task.ProfileSlug, task.Title, task.Description, task.Priority, task)
	}
	return h.resultJSONTracked(project, agent, "promote_task", task)
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
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	task, err := h.db.ReclaimTask(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to reclaim task: %v", err), nil
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
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	task, err := h.db.StartTask(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to start task: %v", err), nil
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
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	existing, err := h.db.GetTask(taskID, project)
	if err != nil || existing == nil {
		return toolResultError("task not found"), nil
	}
	if existing.Status != "blocked" {
		return toolResultError(fmt.Sprintf("task is not blocked (status=%s)", existing.Status)), nil
	}

	task, err := h.db.StartTask(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to resume task: %v", err), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "resume", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.in_progress", "resume", project, task)
	pushStatusAsync(h.getConnector(), task, "in-progress", nil)

	return h.resultJSONTracked(project, agent, "resume_task", task)
}

// HandleLinkPr links a GitHub PR to a task (PR-link S1, DEC-wraith-pr-linking-1).
// Additive + idempotent: omitted fields keep their stored value (COALESCE in
// SetTaskPR), so a re-link or a state-only update never wipes the rest. The
// relay stores the PR zone opaquely — S2's webhook consumer drives status-sync.
func (h *Handlers) HandleLinkPr(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	var prNumber *int
	if n := req.GetInt("pr_number", 0); n > 0 { // GitHub PR numbers start at 1
		prNumber = &n
	}
	prURL := optionalString(req.GetString("pr_url", ""))
	prRepo := optionalString(req.GetString("pr_repo", ""))
	prState := optionalString(req.GetString("pr_state", ""))
	if prState != nil {
		switch *prState {
		case "open", "merged", "closed":
		default:
			return validationError(CodeInvalidArgument, "pr_state must be one of open|merged|closed"), nil
		}
	}
	if prNumber == nil && prURL == nil && prRepo == nil && prState == nil {
		return toolResultError("nothing to link: provide at least one of pr_number, pr_url, pr_repo, pr_state"), nil
	}

	found, err := h.db.SetTaskPR(taskID, project, prURL, prNumber, prState, prRepo)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to link PR: %v", err)), nil
	}
	if !found {
		return validationError(CodeNotFound, fmt.Sprintf("task not found: %s", taskID)), nil
	}

	task, err := h.db.GetTask(taskID, project)
	if err != nil || task == nil {
		return toolResultError(fmt.Sprintf("failed to re-read task: %v", err)), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "link_pr", Agent: agent, Project: project, Label: task.Title})
	prPayload := map[string]any{"doer": agent}
	if task.PRURL != nil {
		prPayload["pr_url"] = *task.PRURL
	}
	if task.PRNumber != nil {
		prPayload["pr_number"] = *task.PRNumber
	}
	if task.PRState != nil {
		prPayload["pr_state"] = *task.PRState
	}
	if task.PRRepo != nil {
		prPayload["pr_repo"] = *task.PRRepo
	}
	emitTaskEvent(h.events, "task.pr_linked", "link_pr", project, task, prPayload)
	return h.resultJSONTracked(project, agent, "link_pr", task)
}

// HandleReconcilePr is the poll-side convergence step (PR-link S3): an external
// poller (niwa, which owns gh) reads relay://pr-reconcile, GETs each PR's live
// state, then calls this with the observed pr_state to converge the task. It
// records the observed state (SetTaskPR, COALESCE — url/repo refreshed if given)
// and applies the SAME status-map the webhook consumer uses (open→in-review,
// merged→done, closed-unmerged→blocked) via ForcePRTransition, which carries the
// no-resurrect + idempotent guards. This is the poll twin of the webhook path —
// no HMAC/webhook-replay coupling — so a missed pull_request webhook still
// converges. The relay stays inbound-only: niwa reaches gh, the relay only
// applies what niwa observed. Returns {task, changed}.
func (h *Handlers) HandleReconcilePr(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	prState := req.GetString("pr_state", "")
	switch prState {
	case "open", "merged", "closed":
	default:
		return validationError(CodeInvalidArgument, "pr_state is required and must be one of open|merged|closed"), nil
	}
	prURL := optionalString(req.GetString("pr_url", ""))
	prRepo := optionalString(req.GetString("pr_repo", ""))

	// Record the observed PR state first (COALESCE keeps number/url/repo; url/repo
	// refreshed only when the poller passes them).
	found, err := h.db.SetTaskPR(taskID, project, prURL, nil, &prState, prRepo)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to record PR state: %v", err)), nil
	}
	if !found {
		return validationError(CodeNotFound, fmt.Sprintf("task not found: %s", taskID)), nil
	}

	target, reason := prTargetFromState(prState)
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	task, changed, err := h.db.ForcePRTransition(project, taskID, target, reasonPtr)
	if err != nil {
		return taskOpError(err, "failed to reconcile PR: %v", err), nil
	}
	if task == nil {
		return validationError(CodeNotFound, fmt.Sprintf("task not found: %s", taskID)), nil
	}
	if changed {
		h.events.Emit(MCPEvent{Type: "task", Action: "reconcile_pr", Agent: agent, Project: project, Label: task.Title})
		name := "task.pr_synced"
		if target == "done" && prState == "merged" {
			name = "task.pr_merged"
		}
		emitTaskEvent(h.events, name, "reconcile_pr", project, task, map[string]any{
			"doer": agent, "pr_state": prState, "reconciled": true,
		})
	}
	return h.resultJSONTracked(project, agent, "reconcile_pr", map[string]any{
		"task": task, "changed": changed,
	})
}

// HandleSetRun stamps the run zone on the PARENT task (changeset-per-factory-run
// S1) — integration_branch and/or a run_state advance. run_state is
// transition-enforced (open-first, no resurrection of a merged run); a bad edge
// returns the typed RUN_STATE_INVALID so the caller parks. Additive + idempotent:
// omitted fields keep their stored value (COALESCE), a same-state stamp is a
// no-op. The relay stores the zone opaquely and stays inbound-only — niwa (S2)
// drives the branch + transitions; the relay never reaches GitHub.
func (h *Handlers) HandleSetRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	integrationBranch := optionalString(req.GetString("integration_branch", ""))
	runState := optionalString(req.GetString("run_state", ""))
	if runState != nil {
		switch *runState {
		case db.RunStateOpen, db.RunStateGating, db.RunStateMerging,
			db.RunStateMerged, db.RunStateBlocked, db.RunStateAmputated:
		default:
			return validationError(CodeInvalidArgument,
				"run_state must be one of open|gating|merging|merged|blocked|amputated"), nil
		}
	}
	if integrationBranch == nil && runState == nil {
		return toolResultError("nothing to set: provide at least one of integration_branch, run_state"), nil
	}

	task, err := h.db.SetTaskRun(taskID, project, integrationBranch, runState)
	if err != nil {
		return taskOpError(err, "failed to set run zone: %v", err), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "set_run", Agent: agent, Project: project, Label: task.Title})
	runPayload := map[string]any{"doer": agent}
	if task.IntegrationBranch != nil {
		runPayload["integration_branch"] = *task.IntegrationBranch
	}
	if task.RunState != nil {
		runPayload["run_state"] = *task.RunState
	}
	emitTaskEvent(h.events, "task.run_updated", "set_run", project, task, runPayload)
	return h.resultJSONTracked(project, agent, "set_run", task)
}

// HandleGetRun returns the run = the PARENT task (carrying the run zone) with its
// subtask chain (the agent slices) attached — the single read S2/niwa and the
// changeset reviewer consume. Pure DB read; the relay stays inbound-only.
func (h *Handlers) HandleGetRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	runID := req.GetString("run_id", "")
	if runID == "" {
		return toolResultError("run_id is required"), nil
	}
	runID, rErr := h.resolveTaskID(runID, project)
	if rErr != nil {
		return toolResultError(rErr.Error()), nil
	}

	run, err := h.db.GetRun(runID, project)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to get run: %v", err)), nil
	}
	if run == nil {
		return toolResultError("run not found"), nil
	}
	return h.resultJSONTracked(project, "", "get_run", run)
}

func (h *Handlers) HandleReviewTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	// Git zone: where the work lives, for the external review gate. Written
	// BEFORE the transition so the re-read task (and its in_review event)
	// carries the fields.
	gitBranch := optionalString(req.GetString("git_branch", ""))
	gitWorktree := optionalString(req.GetString("git_worktree", ""))
	gitTarget := optionalString(req.GetString("git_target", ""))
	if gitBranch != nil || gitWorktree != nil || gitTarget != nil {
		if err := h.db.SetTaskGit(taskID, project, gitBranch, gitWorktree, gitTarget); err != nil {
			return toolResultError(fmt.Sprintf("failed to record git fields: %v", err)), nil
		}
	}

	task, err := h.db.ReviewTask(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to mark task in-review: %v", err), nil
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
		return toolResultError("task_id and body are required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	task, err := h.db.GetTask(taskID, project)
	if err != nil || task == nil {
		return toolResultError("task not found"), nil
	}

	conn := h.getConnector()
	if task.Source == "linear" && conn.Active() && task.LinearIssueID != nil && *task.LinearIssueID != "" {
		if err := conn.Comment(*task.LinearIssueID, body); err != nil {
			return toolResultError(fmt.Sprintf("failed to post comment to Linear: %v", err)), nil
		}
		return h.resultJSONTracked(project, agent, "comment", map[string]any{"posted": "linear", "task_id": taskID})
	}
	if err := h.db.AddProgressNote(taskID, project, agent, body); err != nil {
		return toolResultError(fmt.Sprintf("failed to add note: %v", err)), nil
	}
	return h.resultJSONTracked(project, agent, "comment", map[string]any{"posted": "note", "task_id": taskID})
}

func (h *Handlers) HandleCompleteTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	result := optionalString(req.GetString("result", ""))

	task, err := h.db.CompleteTask(taskID, agent, project, result)
	if err != nil {
		return taskOpError(err, "failed to complete task: %v", err), nil
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
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	reason := optionalString(req.GetString("reason", ""))

	task, err := h.db.BlockTask(taskID, agent, project, reason)
	if err != nil {
		return taskOpError(err, "failed to block task: %v", err), nil
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
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	reason := optionalString(req.GetString("reason", ""))

	task, err := h.db.CancelTask(taskID, agent, project, reason)
	if err != nil {
		return taskOpError(err, "failed to cancel task: %v", err), nil
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
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	title := optionalString(req.GetString("title", ""))
	description := optionalString(req.GetString("description", ""))
	priority := optionalString(req.GetString("priority", ""))
	boardID := optionalString(req.GetString("board_id", ""))
	progressNote := req.GetString("progress_note", "")
	goal := optionalString(req.GetString("goal", ""))
	acceptanceCriteria := optionalString(req.GetString("acceptance_criteria", ""))
	dod := optionalString(req.GetString("dod", ""))

	// The typed-ticket contract (goal/acceptance_criteria/dod) is the review
	// gate's bar for this task. Re-scoping it is a DISPATCHER decision — the
	// assignee doing the work never rewrites their own contract — and every
	// re-scope must land in the audit trail (UpdateTaskFields records it), never
	// silently overwrite it. This used to silently DROP these three fields
	// (neither parsed nor written); refuse explicitly instead.
	if goal != nil || acceptanceCriteria != nil || dod != nil {
		existing, gErr := h.db.GetTask(taskID, project)
		if gErr != nil {
			return toolResultError(fmt.Sprintf("failed to update task: %v", gErr)), nil
		}
		if existing == nil {
			return toolResultError(fmt.Sprintf("task not found: %s", taskID)), nil
		}
		if agent != existing.DispatchedBy {
			return permissionError(CodeForbidden, fmt.Sprintf(
				"goal/acceptance_criteria/dod can only be updated by this task's dispatcher (%s), not the assignee (%s) — re-dispatch instead of re-scoping your own contract",
				existing.DispatchedBy, agent)), nil
		}
		if acceptanceCriteria != nil {
			var items []string
			if err := json.Unmarshal([]byte(*acceptanceCriteria), &items); err != nil {
				return validationError(CodeInvalidArgument, "acceptance_criteria must be a JSON array of testable items"), nil
			}
		}
	}

	task, err := h.db.UpdateTaskFields(taskID, project, agent, title, description, priority, boardID, goal, acceptanceCriteria, dod)
	if err != nil {
		return taskOpError(err, "failed to update task: %v", err), nil
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
		return toolResultError(fmt.Sprintf("failed to archive tasks: %v", err)), nil
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
		return toolResultError("task_id is required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}

	boardID := optionalString(req.GetString("board_id", ""))

	if boardID == nil {
		return toolResultError("board_id is required"), nil
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

	task, err := h.db.UpdateTaskFields(taskID, project, agent, nil, nil, nil, boardID, nil, nil, nil)
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to move task: %v", err)), nil
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
			return toolResultError(fmt.Sprintf("invalid tasks JSON: %v", err)), nil
		}
	}
	if len(items) == 0 {
		return toolResultError("tasks is required — pass tasks:'[{\"task_id\":\"...\",\"result\":\"...\"}]' (JSON string). As a shortcut, task_ids:'[\"id1\",\"id2\"]' is also accepted."), nil
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
		return toolResultError(fmt.Sprintf("invalid tasks JSON: %v", err)), nil
	}
	if len(items) == 0 {
		return toolResultError("tasks is required — pass tasks:'[{\"profile\":\"...\",\"title\":\"...\",\"priority\":\"P2\",\"board_id\":\"...\"}]' (JSON string). Only profile and title are required per item."), nil
	}

	// Typed-ticket enforcement is the single guard in db.DispatchTask (below): on
	// an enforced project a bare item is refused there as a *db.TypedTicketError,
	// lands in errors, and the batch continues (per-item, not all-or-nothing).
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
		priority := item.Priority
		if priority == "" {
			priority = "P2"
		}
		ticket := db.TypedTicket{Goal: item.Goal, AcceptanceCriteria: acJSON, Dod: item.Dod}
		task, err := h.db.DispatchTask(project, item.Profile, agent, item.Title, item.Description, priority, nil, item.BoardID, ticket, false, nil)
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
		return toolResultError("task_id is required"), nil
	}
	taskID, rErr := h.resolveTaskID(taskID, project)
	if rErr != nil {
		return toolResultError(rErr.Error()), nil
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
		return toolResultError(fmt.Sprintf("failed to get task: %v", err)), nil
	}
	if task == nil {
		return toolResultError("task not found"), nil
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
		return toolResultError(fmt.Sprintf("failed to list tasks: %v", err)), nil
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
