package relay

import (
	"agent-relay/internal/db"
	"agent-relay/internal/models"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	profile := strings.ToLower(strings.TrimSpace(req.GetString("profile", "")))
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
	discoveredFrom := strings.TrimSpace(req.GetString("discovered_from", ""))
	if discoveredFrom != "" {
		id, err := h.resolveTaskID(discoveredFrom, project)
		if err != nil {
			return toolResultError(err.Error()), nil
		}
		discoveredFrom = id
	}
	// With discovered_from the profile is inherited from the origin when omitted.
	if profile == "" && discoveredFrom == "" {
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
		VerifyCmd:          optionalString(req.GetString("verify_cmd", "")),
		DiscoveredFrom:     discoveredFrom,
	}
	// blocked_by entries are "<id>" or "<id>@in-review"; a short id prefix
	// resolves like every other task_id argument.
	for _, raw := range req.GetStringSlice("blocked_by", nil) {
		id, until, _ := strings.Cut(strings.TrimSpace(raw), "@")
		if id == "" {
			continue
		}
		full, err := h.resolveTaskID(id, project)
		if err != nil {
			return toolResultError(fmt.Sprintf("blocked_by: %v", err)), nil
		}
		if until != "" {
			full += "@" + until
		}
		ticket.BlockedBy = append(ticket.BlockedBy, full)
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
		return taskOpError(err, "failed to dispatch task: %v", err), nil
	}
	profile = task.ProfileSlug

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

	h.announceDispatched(project, dispatchedBy, profile, title, description, priority, task, backlog)
	return task, autoBoard, nil
}

// announceDispatched emits the signals for a freshly created task. Shared by
// dispatchCore and batch_dispatch_tasks so a batch item behaves exactly as its
// single dispatch_task twin.
func (h *Handlers) announceDispatched(project, dispatchedBy, profile, title, description, priority string, task *models.Task, backlog bool) {
	// A backlog task is groomed-but-not-claimable: emit only the visual event so
	// the board shows it, and SKIP every claim-signal (P0/P1 push, the per-agent
	// inbox delivery, and the task.dispatched event that drives auto-claim). It
	// becomes claimable + surfaced only when promote_task lifts it to pending.
	if backlog {
		h.events.Emit(MCPEvent{Type: "task", Action: "backlog", Agent: dispatchedBy, Project: project, Target: profile, Label: title})
		return
	}

	// A task dispatched blocked_by an unfinished prerequisite is held: pending
	// and visible, but no claim signal until its hold is released, when it is
	// announced exactly once (announceRelease). Same shape as the backlog skip.
	if h.db.TaskHeld(project, task.ID) {
		h.events.Emit(MCPEvent{Type: "task", Action: "held", Agent: dispatchedBy, Project: project, Target: task.ProfileSlug, Label: title})
		return
	}

	h.announceClaimable(project, dispatchedBy, task.ProfileSlug, title, description, priority, task)
}

// announceReleased announces the held tasks a transition released
// (task.Released, settled in the transition's own tx). Called after commit by
// every handler that can release a hold.
func (h *Handlers) announceReleased(project string, ids []string) {
	for _, id := range ids {
		h.announceRelease(project, id)
	}
}

// announceRelease fires the claim signals of one released hold, exactly once:
// the announced_at CAS decides between the inline path and the sweeper, and
// only the winner announces (never read-then-emit). A task that is no longer
// pending by then is marked but not announced. True = announced now.
func (h *Handlers) announceRelease(project, taskID string) bool {
	won, err := h.db.MarkHoldAnnounced(project, taskID, h.db.Now())
	if err != nil || !won {
		return false
	}
	task, err := h.db.GetTask(taskID, project)
	if err != nil || task == nil || task.Status != "pending" || task.ArchivedAt != nil {
		return false
	}
	h.announceClaimable(project, task.DispatchedBy, task.ProfileSlug, task.Title, task.Description, task.Priority, task)
	emitTaskEvent(h.events, "task.released", "release", project, task)
	return true
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
		_, _, _ = h.db.InsertMessageWithDeliveries(project, dispatchedBy, a.Name, "task", subject, content, fmt.Sprintf(`{"task_id":"%s"}`, task.ID), "P2", 14400, nil, nil, []string{a.Name}, "")
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "dispatch", Agent: dispatchedBy, Project: project, Target: profile, Label: title})
	emitTaskEvent(h.events, "task.dispatched", "dispatch", project, task)
}

func (h *Handlers) HandleClaimTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	release := req.GetBool("release", false)
	if req.GetBool("next", false) && taskID == "" && !release {
		return h.claimNext(project, agent, req.GetString("sort", db.SortPriority))
	}
	if taskID == "" {
		return toolResultError("task_id is required (or next=true)"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	if release {
		return h.releaseTask(project, agent, taskID)
	}

	// Readiness is a projection, not a gate (ruling b3a6ab43 OQ3): a claim of a
	// task with unsatisfied prerequisites succeeds, and the response names
	// them. Read before the claim: once accepted the task is no longer pending.
	_, blockers, _ := h.db.TaskReadiness(project, taskID)

	claim := h.db.ClaimTask
	if req.GetBool("force", false) {
		claim = h.db.ClaimTaskForce
	}
	task, err := claim(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to claim task: %v", err), nil
	}
	return h.claimed(project, agent, task, blockers)
}

// releaseTask hands an accepted, not-started task back to pending
// (claim_task release=true): holder or dispatcher only. The task re-enters the
// claimable pool, so it is announced like a fresh dispatch unless a
// prerequisite still holds it (the release then comes with the hold).
func (h *Handlers) releaseTask(project, agent, taskID string) (*mcp.CallToolResult, error) {
	task, err := h.db.ReleaseTask(taskID, agent, project)
	if errors.Is(err, db.ErrTaskReleaseForbidden) {
		return validationError(CodeForbidden, err.Error()), nil
	}
	if err != nil {
		return taskOpError(err, "failed to release task: %v", err), nil
	}
	if task.LeaseTransfer != nil {
		emitTaskEvent(h.events, "task.lease_transferred", "release", project, task, map[string]any{
			"from":   task.LeaseTransfer.From,
			"to":     task.LeaseTransfer.To,
			"reason": task.LeaseTransfer.Reason,
		})
	}
	pushStatusAsync(h.getConnector(), task, "pending", nil)
	if !h.db.TaskHeld(project, task.ID) {
		h.announceClaimable(project, agent, task.ProfileSlug, task.Title, task.Description, task.Priority, task)
	}
	return h.resultJSONTracked(project, agent, "claim_task", task)
}

// claimed emits the claim signals and renders the claim_task response, with
// the readiness warning when the task had unsatisfied prerequisites.
func (h *Handlers) claimed(project, agent string, task *models.Task, blockers []models.EdgeRef) (*mcp.CallToolResult, error) {
	h.events.Emit(MCPEvent{Type: "task", Action: "claim", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.claimed", "claim", project, task)
	pushStatusAsync(h.getConnector(), task, "accepted", nil)
	// Basis stamp in its own tx after the transition (design 54e529d8 §4.3).
	basis := h.stampBasis(project, agent, db.BasisClaim, []string{task.ID})
	out := h.withStaleContext(project, agent, withBasis(task, basis))
	if m, ok := out.(map[string]any); ok && len(blockers) > 0 {
		list := make([]map[string]any, 0, len(blockers))
		for _, b := range blockers {
			entry := map[string]any{"id": b.ID, "status": b.Status, "until": b.Until}
			if p, _ := h.db.GetTask(b.ID, project); p != nil {
				entry["title"] = p.Title
			}
			list = append(list, entry)
		}
		m["readiness"] = map[string]any{"ready": false, "blockers": list}
	}
	return h.resultJSONTracked(project, agent, "claim_task", out)
}

// claimNext claims the caller's next ready task (claim_task next=true): only
// tasks routed to the caller's registered profile, through the one readiness
// predicate, walking past candidates a racing claimer took.
func (h *Handlers) claimNext(project, agent, sortBy string) (*mcp.CallToolResult, error) {
	switch sortBy {
	case db.SortPriority, db.SortOldest, db.SortUnblockImpact:
	default:
		return validationError(CodeInvalidArgument, fmt.Sprintf("sort must be priority, oldest or unblock_impact (got %q)", sortBy)), nil
	}
	a, err := h.db.GetAgent(project, agent)
	if err != nil || a == nil || a.ProfileSlug == nil || *a.ProfileSlug == "" {
		return validationError(CodeInvalidArgument, "next=true claims from your registered profile; register with profile_slug or pass task_id"), nil
	}
	task, err := h.db.ClaimNextTask(project, agent, *a.ProfileSlug, "", sortBy)
	if err != nil {
		return taskOpError(err, "failed to claim next task: %v", err), nil
	}
	if task == nil {
		ready, _ := h.db.ListReadyTasks(project, *a.ProfileSlug, "", 0)
		return h.resultJSONTracked(project, agent, "claim_task", map[string]any{"task": nil, "ready_count": len(ready)})
	}
	return h.claimed(project, agent, task, nil)
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

// HandleDemoteTask sends pending native tasks back to backlog (park P1
// 11300e39, design park.md §3): task_id for one, task_ids for a batch where
// each id gets its own result and one refusal never aborts the rest. Authority
// is callerMayReassign (dispatcher, executive, or the doer's lead chain); a
// Linear mirror is refused with a hint naming park_task.
func (h *Handlers) HandleDemoteTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := strings.ToLower(resolveAgent(ctx, req))
	reason := strings.TrimSpace(req.GetString("reason", ""))
	if reason == "" {
		return validationError(CodeInvalidArgument, "reason is required"), nil
	}
	single := req.GetString("task_id", "")
	ids, errMsg := demoteTaskIDs(req)
	if errMsg != "" {
		return validationError(CodeInvalidArgument, errMsg), nil
	}
	if (single == "") == (len(ids) == 0) {
		return validationError(CodeInvalidArgument, "pass exactly one of task_id or task_ids"), nil
	}
	if single != "" {
		task, _, refusal := h.demoteOne(project, agent, single, reason)
		if refusal != nil {
			return refusal, nil
		}
		return h.resultJSONTracked(project, agent, "demote_task", task)
	}

	type itemResult struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"` // demoted | unchanged | refused
		Error  string `json:"error,omitempty"`
	}
	results := make([]itemResult, 0, len(ids))
	demoted := 0
	for _, id := range ids {
		task, changed, refusal := h.demoteOne(project, agent, id, reason)
		switch {
		case refusal != nil:
			results = append(results, itemResult{TaskID: id, Status: "refused", Error: toolErrorMessage(refusal)})
		case changed:
			demoted++
			results = append(results, itemResult{TaskID: task.ID, Status: "demoted"})
		default:
			results = append(results, itemResult{TaskID: task.ID, Status: "unchanged"})
		}
	}
	return h.resultJSONTracked(project, agent, "demote_task", map[string]any{
		"results": results,
		"demoted": demoted,
		"total":   len(ids),
	})
}

// demoteOne demotes one task id (short prefixes resolve). It returns the task
// and whether it moved, or the typed refusal.
func (h *Handlers) demoteOne(project, agent, rawID, reason string) (*models.Task, bool, *mcp.CallToolResult) {
	taskID, err := h.resolveTaskID(rawID, project)
	if err != nil {
		return nil, false, validationError(CodeNotFound, err.Error())
	}
	task, err := h.db.GetTask(taskID, project)
	if err != nil {
		return nil, false, taskOpError(err, "failed to load task: %v", err)
	}
	if task == nil {
		return nil, false, validationError(CodeNotFound, fmt.Sprintf("task not found: %s", rawID))
	}
	if task.Source == "linear" {
		return nil, false, toolError(CodeForbidden, CategoryPermission, false,
			fmt.Sprintf("task %s is mirrored from Linear (Linear is the source of truth): demote cannot move it — use park_task to freeze it", taskID),
			map[string]any{"hint": "park_task"})
	}
	if !h.callerMayReassign(project, agent, task) {
		return nil, false, toolError(CodeForbidden, CategoryPermission, false,
			fmt.Sprintf("%q may not demote task %s: only its dispatcher (%s), an executive, or the doer's lead chain", agent, taskID, task.DispatchedBy), nil)
	}
	updated, changed, err := h.db.DemoteTask(taskID, agent, project, reason)
	if err != nil {
		return nil, false, taskOpError(err, "failed to demote task: %v", err)
	}
	return updated, changed, nil
}

// toolErrorMessage returns the "message" of a typed tool-error result (the
// raw text when it is not the JSON envelope), for per-item batch reports.
func toolErrorMessage(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	text, _ := res.Content[0].(mcp.TextContent)
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(text.Text), &body) == nil && body.Message != "" {
		return body.Message
	}
	return text.Text
}

// demoteTaskIDs reads task_ids as a native array or a JSON-array string.
func demoteTaskIDs(req mcp.CallToolRequest) ([]string, string) {
	raw, given := req.GetArguments()["task_ids"]
	if !given || raw == nil {
		return nil, ""
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, ""
		}
		if err := json.Unmarshal([]byte(v), &items); err != nil {
			return nil, fmt.Sprintf("task_ids must be an array of task ids (got %q)", v)
		}
	default:
		return nil, fmt.Sprintf("task_ids must be an array of task ids (got %#v)", raw)
	}
	ids := make([]string, 0, len(items))
	for i, el := range items {
		s, ok := el.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Sprintf("task_ids[%d] must be a non-empty string (got %#v)", i, el)
		}
		ids = append(ids, strings.TrimSpace(s))
	}
	return ids, ""
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

	// start_task from pending is an implicit claim: it gets a claim stamp.
	fromPending := false
	if prev, _ := h.db.GetTask(taskID, project); prev != nil && prev.Status == "pending" {
		fromPending = true
	}
	task, err := h.db.StartTask(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to start task: %v", err), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "start", Agent: agent, Project: project, Label: task.Title})
	emitTaskEvent(h.events, "task.in_progress", "start", project, task)
	pushStatusAsync(h.getConnector(), task, "in-progress", nil)
	if fromPending {
		basis := h.stampBasis(project, agent, db.BasisClaim, []string{task.ID})
		return h.resultJSONTracked(project, agent, "start_task", h.withStaleContext(project, agent, withBasis(task, basis)))
	}
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
	if existing.Status == "pending" && h.db.TaskParked(project, taskID) {
		task, res := h.parkTarget(project, agent, taskID, "resume")
		if res != nil {
			return res, nil
		}
		return h.unpark(project, agent, task)
	}
	if existing.Status != "blocked" {
		return validationError(CodeInvalidArgument, fmt.Sprintf("task is not blocked (status=%s)", existing.Status)), nil
	}

	task, err := h.db.ResumeTask(taskID, agent, project)
	if err != nil {
		return taskOpError(err, "failed to resume task: %v", err), nil
	}
	h.events.Emit(MCPEvent{Type: "task", Action: "resume", Agent: agent, Project: project, Label: task.Title})
	ev := EvTaskInProgress
	if task.Status == "accepted" { // blocked while accepted: back to its claim
		ev = EvTaskClaimed
	}
	emitTaskEvent(h.events, ev, "resume", project, task)
	pushStatusAsync(h.getConnector(), task, task.Status, nil)

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

	gen, invalid := leaseGenerationArg(req)
	if invalid != nil {
		return invalid, nil
	}
	task, err := h.db.ReviewTaskFenced(taskID, agent, project, gen)
	if err != nil {
		return taskOpError(err, "failed to mark task in-review: %v", err), nil
	}
	h.announceReleased(project, task.Released)
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
	// A secondary fan-out mirror never writes back to Linear (only the primary
	// mirror does — see db.LinearMirrorSeed.Secondary); its comment falls through
	// to the local progress-note path below instead of being silently dropped.
	if task.Source == "linear" && !task.LinearSecondary && conn.Active() && task.LinearIssueID != nil && *task.LinearIssueID != "" {
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
	gen, invalid := leaseGenerationArg(req)
	if invalid != nil {
		return invalid, nil
	}

	task, err := h.db.CompleteTaskFenced(taskID, agent, project, result, gen)
	if err != nil {
		return taskOpError(err, "failed to complete task: %v", err), nil
	}
	h.announceReleased(project, task.Released)

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

	basis := h.stampBasis(project, agent, db.BasisComplete, []string{task.ID})
	return h.resultJSONTracked(project, agent, "complete_task", withBasis(task, basis))
}

// leaseGenerationArg reads the optional lease_generation of complete / block /
// review (S3 0b980988). Absent = nil (holder check only, unless
// RELAY_STRICT_FENCING=1); a non-integer is refused INVALID_ARGUMENT before
// any write.
func leaseGenerationArg(req mcp.CallToolRequest) (*int64, *mcp.CallToolResult) {
	raw, given := req.GetArguments()["lease_generation"]
	if !given || raw == nil {
		return nil, nil
	}
	f, ok := raw.(float64)
	if !ok || f != float64(int64(f)) || f < 0 {
		return nil, validationError(CodeInvalidArgument, "lease_generation must be a non-negative integer")
	}
	g := int64(f)
	return &g, nil
}

// declaredReasonCode reads the optional reason_code of block_task / cancel_task.
// A value outside db.DeclaredReasonCodes is refused INVALID_ARGUMENT before any
// write (design dbc317f4 §4); "" means none declared.
func declaredReasonCode(req mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	code := strings.TrimSpace(req.GetString("reason_code", ""))
	if code != "" && !db.IsDeclaredReasonCode(code) {
		return "", validationError(CodeInvalidArgument, "reason_code must be one of "+strings.Join(db.DeclaredReasonCodes, "|"))
	}
	return code, nil
}

func (h *Handlers) HandleBlockTask(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	taskID := req.GetString("task_id", "")
	if taskID == "" {
		return toolResultError("task_id is required"), nil
	}
	reasonCode, invalid := declaredReasonCode(req)
	if invalid != nil {
		return invalid, nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	reason := optionalString(req.GetString("reason", ""))
	gen, invalid := leaseGenerationArg(req)
	if invalid != nil {
		return invalid, nil
	}

	task, err := h.db.BlockTaskFenced(taskID, agent, project, reason, reasonCode, gen)
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
	reasonCode, invalid := declaredReasonCode(req)
	if invalid != nil {
		return invalid, nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	reason := optionalString(req.GetString("reason", ""))

	task, err := h.db.CancelTaskWithCode(taskID, agent, project, reason, reasonCode)
	if err != nil {
		return taskOpError(err, "failed to cancel task: %v", err), nil
	}
	h.announceReleased(project, task.Released)
	cancelledExtra := map[string]any{}
	if reason != nil {
		cancelledExtra["reason"] = *reason
	}
	emitTaskEvent(h.events, "task.cancelled", "cancel", project, task, cancelledExtra)
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

	// AC2 / rule (e): a field update_task does not recognise is refused loudly,
	// naming it — never silently ignored with a 200 + unchanged record (the
	// original defect that dropped profile_slug/assigned_to). A status change is
	// not a field edit: it has its own verbs.
	for k := range req.GetArguments() {
		if !updateTaskArgs[k] {
			return validationError(CodeInvalidArgument, fmt.Sprintf(
				"%q is not an updatable field of update_task — updatable: title, description, priority, board_id, assigned_to, profile_slug, progress_note, goal, acceptance_criteria, dod, verify_cmd, blocked_by, blocked_by_remove (change status with start_task/complete_task/block_task/cancel_task/resume_task/review_task)",
				k)), nil
		}
	}

	// Typed-field validation (silent-drop fix, task 88510cb5). GetString silently
	// falls back to "" when an arg is present but the wrong JSON type, so a
	// mistyped value used to be DROPPED with a 200 + bumped last_activity_at and
	// no error (live repro 3ad80aae: acceptance_criteria as a native array kept
	// the old ACs and dropped the progress_note in the same call). Refuse every
	// string field that is present with a non-string value — atomically, BEFORE
	// any write, naming the field and the expected type. acceptance_criteria
	// additionally accepts a native array of strings, coerced to its canonical
	// JSON-string form, for parity with batch_dispatch_tasks' ergonomics.
	args := req.GetArguments()
	for _, f := range []string{"title", "description", "priority", "board_id", "assigned_to", "profile_slug", "progress_note", "goal", "dod", "verify_cmd"} {
		if raw, given := args[f]; given {
			if _, ok := raw.(string); !ok {
				return validationError(CodeInvalidArgument, fmt.Sprintf(
					"%s must be a string (got %#v)", f, raw)), nil
			}
		}
	}
	var acOverride *string
	if raw, given := args["acceptance_criteria"]; given {
		switch v := raw.(type) {
		case string:
			// A JSON-string value keeps the existing JSON-array validation below.
		case []any:
			items := make([]string, 0, len(v))
			for i, el := range v {
				s, ok := el.(string)
				if !ok {
					return validationError(CodeInvalidArgument, fmt.Sprintf(
						"acceptance_criteria[%d] must be a string (got %#v) — the array is neither coerced nor dropped", i, el)), nil
				}
				items = append(items, s)
			}
			canon, mErr := json.Marshal(items)
			if mErr != nil {
				return validationError(CodeInvalidArgument, fmt.Sprintf("acceptance_criteria could not be encoded: %v", mErr)), nil
			}
			s := string(canon)
			acOverride = &s
		default:
			return validationError(CodeInvalidArgument, fmt.Sprintf(
				"acceptance_criteria must be a JSON string or an array of strings (got %#v)", raw)), nil
		}
	}

	assignedTo := optionalStringLower(strings.TrimSpace(req.GetString("assigned_to", "")))
	profileSlug := optionalStringLower(strings.TrimSpace(req.GetString("profile_slug", "")))

	title := optionalString(req.GetString("title", ""))
	description := optionalString(req.GetString("description", ""))
	priority := optionalString(req.GetString("priority", ""))
	boardID := optionalString(req.GetString("board_id", ""))
	progressNote := req.GetString("progress_note", "")
	goal := optionalString(req.GetString("goal", ""))
	acceptanceCriteria := acOverride
	if acceptanceCriteria == nil {
		acceptanceCriteria = optionalString(req.GetString("acceptance_criteria", ""))
	}
	dod := optionalString(req.GetString("dod", ""))
	verifyCmd := optionalString(req.GetString("verify_cmd", ""))

	// The typed-ticket contract (goal/acceptance_criteria/dod) is the review
	// gate's bar for this task. Re-scoping it is a DISPATCHER decision — the
	// assignee doing the work never rewrites their own contract — and every
	// re-scope must land in the audit trail (UpdateTaskFields records it), never
	// silently overwrite it. This used to silently DROP these three fields
	// (neither parsed nor written); refuse explicitly instead. verify_cmd rides
	// the same gate — it is the same dispatcher-owned contract surface (task
	// 6c1c5167 follow-up, DEC-niwa-goal-validate-1) even though its absence is
	// never enforced.
	var contractBefore *models.Task
	contractAuthority := ""
	if goal != nil || acceptanceCriteria != nil || dod != nil || verifyCmd != nil {
		existing, gErr := h.db.GetTask(taskID, project)
		if gErr != nil {
			return toolResultError(fmt.Sprintf("failed to update task: %v", gErr)), nil
		}
		if existing == nil {
			return toolResultError(fmt.Sprintf("task not found: %s", taskID)), nil
		}
		authority, refusal := h.contractEditAuthority(project, agent, existing)
		if refusal != nil {
			return refusal, nil
		}
		contractBefore, contractAuthority = existing, authority
		if acceptanceCriteria != nil {
			var items []string
			if err := json.Unmarshal([]byte(*acceptanceCriteria), &items); err != nil {
				return validationError(CodeInvalidArgument, "acceptance_criteria must be a JSON array of testable items"), nil
			}
		}
	}

	// blocked_by edits first: a refusal (not the dispatcher, cycle, unknown id)
	// then lands before any field write.
	if res := h.updateBlockedBy(project, agent, taskID, req.GetStringSlice("blocked_by", nil), req.GetStringSlice("blocked_by_remove", nil)); res != nil {
		return res, nil
	}

	hasFieldEdit := title != nil || description != nil || priority != nil || boardID != nil ||
		goal != nil || acceptanceCriteria != nil || dod != nil || verifyCmd != nil

	var task *models.Task
	if hasFieldEdit {
		task, err = h.db.UpdateTaskFields(taskID, project, agent, title, description, priority, boardID, goal, acceptanceCriteria, dod, verifyCmd)
		if err != nil {
			return taskOpError(err, "failed to update task: %v", err), nil
		}
	}
	if contractBefore != nil && task != nil {
		h.recordContractEdit(project, agent, contractAuthority, contractBefore, task, goal, acceptanceCriteria, dod, verifyCmd)
	}

	// assigned_to / profile_slug REASSIGN the task without changing its status
	// (DEC-wraith-update-task-reassign-1). Handled after the plain field edits so a
	// single call may edit fields and hand the task on in one shot.
	if assignedTo != nil || profileSlug != nil {
		reassigned, res := h.reassignViaUpdate(project, agent, taskID, assignedTo, profileSlug)
		if res != nil {
			return res, nil
		}
		task = reassigned
	}

	if task == nil {
		// progress_note-only (or empty) update: nothing was written above, so
		// re-read the row to still return the current record.
		task, err = h.db.GetTask(taskID, project)
		if err != nil {
			return toolResultError(fmt.Sprintf("failed to update task: %v", err)), nil
		}
		if task == nil {
			return toolResultError(fmt.Sprintf("task not found: %s", taskID)), nil
		}
	}

	if progressNote != "" {
		if err := h.db.AddProgressNote(taskID, project, agent, progressNote); err == nil {
			h.events.Emit(MCPEvent{Type: "task", Action: "progress", Agent: agent, Project: project, Label: task.Title})
		}
	}

	h.events.Emit(MCPEvent{Type: "task", Action: "update", Agent: agent, Project: project, Label: task.Title})
	return h.resultJSONTracked(project, agent, "update_task", task)
}

// updateTaskArgs is the whitelist of arguments update_task recognises. A key
// outside it is refused (rule (e)) rather than silently ignored — as/project/
// task_id are transport, the rest are the updatable fields.
var updateTaskArgs = map[string]bool{
	"as": true, "project": true, "task_id": true,
	"title": true, "description": true, "priority": true, "board_id": true,
	"assigned_to": true, "profile_slug": true, "progress_note": true,
	"goal": true, "acceptance_criteria": true, "dod": true, "verify_cmd": true,
	"blocked_by": true, "blocked_by_remove": true,
}

// reassignViaUpdate applies the assigned_to/profile_slug reassignment path of
// update_task per DEC-wraith-update-task-reassign-1. It returns either the
// updated task, or a non-nil tool result to hand straight back (a permission or
// validation refusal). caller is the acting agent.
func (h *Handlers) reassignViaUpdate(project, caller, taskID string, assignedTo, profileSlug *string) (*models.Task, *mcp.CallToolResult) {
	existing, err := h.db.GetTask(taskID, project)
	if err != nil {
		return nil, toolResultError(fmt.Sprintf("failed to update task: %v", err))
	}
	if existing == nil {
		return nil, toolResultError(fmt.Sprintf("task not found: %s", taskID))
	}
	if existing.Source == "linear" {
		return nil, validationError(CodeInvalidArgument,
			"task is mirrored from Linear (read-only here — Linear is the source of truth)")
	}

	// (d) Only the dispatcher, an agent in the assignee's lead chain, or an
	// executive may reassign — a doer cannot reassign its own task.
	if !h.callerMayReassign(project, caller, existing) {
		return nil, permissionError(CodeForbidden, fmt.Sprintf(
			"only this task's dispatcher (%s), an agent in its lead chain, or an executive may reassign it — a doer cannot reassign its own task",
			existing.DispatchedBy))
	}

	switch existing.Status {
	case "pending":
		// (c) On a pending task both fields update freely; no lease is minted.
		task, rErr := h.db.ReassignTaskFields(taskID, project, caller, assignedTo, profileSlug, false)
		if rErr != nil {
			return nil, taskOpError(rErr, "failed to reassign task: %v", rErr)
		}
		return task, nil
	case "accepted", "in-progress", "in-review":
		holder := taskHolder(existing)
		if assignedTo == nil {
			// (b) A profile_slug change alone on a claimed task is refused — never
			// silently re-profile held work.
			return nil, validationError(CodeInvalidArgument, fmt.Sprintf(
				"task is claimed by %s; pass assigned_to to transfer the lease, or have %s release it", holder, holder))
		}
		// (a) assigned_to on a claimed task = atomic lease transfer.
		task, rErr := h.db.ReassignTaskFields(taskID, project, caller, assignedTo, profileSlug, true)
		if rErr != nil {
			return nil, taskOpError(rErr, "failed to reassign task: %v", rErr)
		}
		if holder != "" && !strings.EqualFold(holder, *assignedTo) {
			h.registry.Notify(project, holder, caller,
				fmt.Sprintf("Task reassigned to %s by %s: %s", *assignedTo, caller, task.Title), taskID)
		}
		return task, nil
	default:
		return nil, validationError(CodeInvalidArgument, fmt.Sprintf(
			"cannot reassign a %s task — reassignment applies to a pending or held (accepted/in-progress/in-review) task", existing.Status))
	}
}

// taskHolder is the agent currently holding a task: its claimed_by, else its
// lease_holder, else "".
func taskHolder(t *models.Task) string {
	if t.ClaimedBy != nil && *t.ClaimedBy != "" {
		return *t.ClaimedBy
	}
	if t.LeaseHolder != nil && *t.LeaseHolder != "" {
		return *t.LeaseHolder
	}
	return ""
}

// callerMayReassign reports whether caller may hand this task to another
// profile/agent (DEC rule (d)): the task's dispatcher, any executive, or an
// agent in the doer's lead chain (a reports_to ancestor of the assignee). A
// plain doer — including the task's own holder — is not, so it cannot reassign
// its own work. Agent names are stored lowercase, so comparisons fold case.
func (h *Handlers) callerMayReassign(project, caller string, task *models.Task) bool {
	if caller == "" {
		return false
	}
	if strings.EqualFold(caller, task.DispatchedBy) {
		return true
	}
	return h.callerIsContractSigner(project, caller, task)
}

// contractEditAuthority decides who may edit a task's typed-ticket contract
// (goal/acceptance_criteria/dod/verify_cmd) and returns the authority used, or
// the refusal (04ac0ae3):
//   - never the doer (holder or assignee) nor an agent below it in the
//     reports_to chain, even an executive (anti-fab b9efdfb4);
//   - the dispatcher (not on a self-dispatched task: DEC-wraith-self-grading-guard-1);
//   - an executive;
//   - on a self-dispatched task, the doer's lead chain (sign-off from above);
//   - once the dispatcher is inactive (dead, deactivated, unregistered), an
//     agent above it in its reports_to chain — so a dead dispatcher no longer
//     freezes its tickets' contract.
func (h *Handlers) contractEditAuthority(project, caller string, task *models.Task) (string, *mcp.CallToolResult) {
	caller = strings.ToLower(caller)
	dispatcher := strings.ToLower(task.DispatchedBy)
	selfDispatched := task.AssignedTo != nil && strings.EqualFold(task.DispatchedBy, *task.AssignedTo)
	doer := strings.ToLower(taskHolder(task))
	if doer == "" && task.AssignedTo != nil {
		doer = strings.ToLower(*task.AssignedTo)
	}
	if doer != "" && caller == doer {
		if selfDispatched {
			return "", permissionError(CodeForbidden, fmt.Sprintf(
				"goal/acceptance_criteria/dod/verify_cmd on a self-dispatched task can't be rewritten by the doer who dispatched it to itself (%s) — that is self-grading. It needs sign-off from above you: an executive, or an agent in your reports_to lead chain (DEC-wraith-self-grading-guard-1)",
				caller))
		}
		return "", permissionError(CodeForbidden, fmt.Sprintf(
			"%s holds this task: the doer never rewrites its own contract (goal/acceptance_criteria/dod/verify_cmd), even as an executive — ask its dispatcher (%s) or an executive",
			caller, task.DispatchedBy))
	}
	if doer != "" && caller != "" && h.reportsUpTo(project, caller, doer) {
		return "", permissionError(CodeForbidden, fmt.Sprintf(
			"%s reports to %s, who holds this task: nobody below the doer rewrites its contract", caller, doer))
	}
	switch {
	case caller == "":
	case !selfDispatched && caller == dispatcher:
		return "dispatcher", nil
	case h.callerIsExecutive(project, caller):
		return "executive", nil
	case selfDispatched && h.callerIsContractSigner(project, caller, task):
		return "doer_lead_chain", nil
	case !h.agentIsLive(project, dispatcher) && h.reportsUpTo(project, dispatcher, caller):
		return "dispatcher_lead_chain", nil
	}
	if selfDispatched {
		return "", permissionError(CodeForbidden, fmt.Sprintf(
			"goal/acceptance_criteria/dod/verify_cmd on a self-dispatched task need sign-off from above its doer (%s): an executive, or an agent in its reports_to lead chain (DEC-wraith-self-grading-guard-1)",
			task.DispatchedBy))
	}
	return "", permissionError(CodeForbidden, fmt.Sprintf(
		"goal/acceptance_criteria/dod/verify_cmd can only be updated by this task's dispatcher (%s), an executive, or — once %s is inactive — an agent above it in its reports_to chain; never by the assignee",
		task.DispatchedBy, task.DispatchedBy))
}

// callerIsExecutive reports whether caller is a registered executive.
func (h *Handlers) callerIsExecutive(project, caller string) bool {
	ag, _ := h.db.GetAgent(project, caller)
	return ag != nil && ag.IsExecutive
}

// agentIsLive reports whether name is registered and active or sleeping (the
// sender liveness rule, db.SenderEligibility).
func (h *Handlers) agentIsLive(project, name string) bool {
	ag, _ := h.db.GetAgent(project, name)
	live, _ := db.SenderEligibility(ag)
	return ag != nil && live
}

// reportsUpTo reports whether `from`'s reports_to chain reaches `to` (from
// excluded). Bounded by a seen-set against a cyclic chain.
func (h *Handlers) reportsUpTo(project, from, to string) bool {
	seen := map[string]bool{}
	cur := strings.ToLower(from)
	for cur != "" && !seen[cur] {
		seen[cur] = true
		ag, err := h.db.GetAgent(project, cur)
		if err != nil || ag == nil || ag.ReportsTo == nil {
			return false
		}
		cur = strings.ToLower(*ag.ReportsTo)
		if cur == strings.ToLower(to) {
			return true
		}
	}
	return false
}

// contractHash fingerprints a task's typed-ticket contract for the audit trail.
func contractHash(t *models.Task) string {
	verify := ""
	if t.VerifyCmd != nil {
		verify = *t.VerifyCmd
	}
	b, _ := json.Marshal([]string{t.Goal, t.AcceptanceCriteria, t.Dod, verify})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// recordContractEdit audits a contract edit as task.contract_edited {by,
// authority, fields, before_hash, after_hash} and, when the task is in review,
// posts a progress note so the gate's AC refresh picks the new contract up.
func (h *Handlers) recordContractEdit(project, by, authority string, before, after *models.Task, goal, ac, dod, verify *string) {
	var fields []string
	for _, f := range []struct {
		name string
		v    *string
	}{{"goal", goal}, {"acceptance_criteria", ac}, {"dod", dod}, {"verify_cmd", verify}} {
		if f.v != nil {
			fields = append(fields, f.name)
		}
	}
	beforeHash, afterHash := contractHash(before), contractHash(after)
	details, _ := json.Marshal(map[string]any{
		"by": by, "authority": authority, "fields": fields,
		"before_hash": beforeHash, "after_hash": afterHash,
	})
	if err := h.db.RecordAudit(models.AuditEntry{
		Project:      project,
		Actor:        by,
		Action:       "task.contract_edited",
		ResourceType: "task",
		ResourceID:   after.ID,
		Summary:      fmt.Sprintf("%s edited %s (%s)", by, strings.Join(fields, ", "), authority),
		Details:      string(details),
	}); err != nil {
		log.Printf("contract edit audit %s: %v", after.ID, err)
	}
	if before.Status == "in-review" {
		note := fmt.Sprintf("contract edited by %s (%s): %s changed (%s → %s) — the review must re-read the acceptance criteria",
			by, authority, strings.Join(fields, ", "), beforeHash, afterHash)
		if err := h.db.AddProgressNote(after.ID, project, by, note); err != nil {
			log.Printf("contract edit note %s: %v", after.ID, err)
		}
	}
}

// callerIsContractSigner reports whether caller is a legitimate sign-off
// authority ABOVE the doer of this task: an executive, or an agent in the doer's
// reports_to lead chain (an ancestor lead). The doer itself is never a signer —
// that is the point on a self-dispatched task, where the doer is also the
// dispatcher (DEC-wraith-self-grading-guard-1). Unlike callerMayReassign it
// carries NO dispatcher shortcut, so a self-dispatched doer cannot use it to
// clear its own contract. Agent names are stored lowercase, so comparisons fold
// case. Bounded by a seen-set against a cyclic reports_to chain.
func (h *Handlers) callerIsContractSigner(project, caller string, task *models.Task) bool {
	if caller == "" {
		return false
	}
	if ag, _ := h.db.GetAgent(project, strings.ToLower(caller)); ag != nil && ag.IsExecutive {
		return true
	}
	cur := strings.ToLower(taskHolder(task))
	if cur == "" && task.AssignedTo != nil {
		cur = strings.ToLower(*task.AssignedTo)
	}
	seen := map[string]bool{}
	for cur != "" && !seen[cur] {
		seen[cur] = true
		ag, err := h.db.GetAgent(project, cur)
		if err != nil || ag == nil || ag.ReportsTo == nil || *ag.ReportsTo == "" {
			break
		}
		lead := strings.ToLower(*ag.ReportsTo)
		if strings.EqualFold(lead, caller) {
			return true
		}
		cur = lead
	}
	return false
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

	task, err := h.db.UpdateTaskFields(taskID, project, agent, nil, nil, nil, boardID, nil, nil, nil, nil)
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

	type batchItem struct {
		TaskID          string  `json:"task_id"`
		Result          *string `json:"result"`
		LeaseGeneration *int64  `json:"lease_generation"`
	}
	var items []batchItem
	// Accept the common mistake task_ids:["..."] as a shorthand for
	// tasks:[{task_id:"..."}] (no result).
	if tasksJSON == "" {
		if idsJSON := req.GetString("task_ids", ""); idsJSON != "" {
			var ids []string
			if err := json.Unmarshal([]byte(idsJSON), &ids); err == nil {
				for _, id := range ids {
					items = append(items, batchItem{TaskID: id})
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
		task, err := h.db.CompleteTaskFenced(taskID, agent, project, item.Result, item.LeaseGeneration)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", taskID, err))
			continue
		}
		completed = append(completed, taskID)
		h.events.Emit(MCPEvent{Type: "task", Action: "complete", Agent: agent, Project: project, Label: task.Title})
		h.announceReleased(project, task.Released)
	}

	// One stamp tx for the whole batch.
	basis := h.stampBasis(project, agent, db.BasisComplete, completed)
	return h.resultJSONTracked(project, agent, "batch_complete_tasks", map[string]any{
		"completed": completed,
		"errors":    errors,
		"total":     len(items),
		"basis":     basis,
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
		VerifyCmd          *string  `json:"verify_cmd"`
		Backlog            bool     `json:"backlog"`
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
		ticket := db.TypedTicket{Goal: item.Goal, AcceptanceCriteria: acJSON, Dod: item.Dod, VerifyCmd: item.VerifyCmd}
		task, err := h.db.DispatchTask(project, item.Profile, agent, item.Title, item.Description, priority, nil, item.BoardID, ticket, item.Backlog, nil)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", item.Title, err))
			continue
		}
		dispatched = append(dispatched, map[string]string{"id": task.ID, "title": task.Title})
		h.announceDispatched(project, agent, item.Profile, item.Title, item.Description, priority, task, item.Backlog)
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
	one := []models.Task{*task}
	if err := h.db.AttachParks(project, one); err != nil {
		return toolResultError(fmt.Sprintf("failed to get task: %v", err)), nil
	}

	return h.resultJSONTracked(project, "", "get_task", &one[0])
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

	var tasks []models.Task
	var err error
	if req.GetBool("ready", false) {
		// The one readiness predicate (same as claim next=true); ready implies
		// pending and not archived, so status and include_archived do not apply.
		tasks, err = h.db.ListReadyTasks(project, profile, boardID, limit)
	} else {
		tasks, err = h.db.ListTasks(project, status, profile, priority, assignedTo, boardID, limit, includeArchived)
	}
	if err != nil {
		return toolResultError(fmt.Sprintf("failed to list tasks: %v", err)), nil
	}
	if tasks == nil {
		tasks = []models.Task{}
	}
	if err := h.db.AttachParks(project, tasks); err != nil {
		return toolResultError(fmt.Sprintf("failed to list tasks: %v", err)), nil
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
			status := t.Status
			if t.Park != nil {
				status += " (parked until " + t.Park.Until + ")"
			}
			rows[i] = []string{
				t.ID, status, t.Priority, t.ProfileSlug, strOrDash(t.AssignedTo),
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

// HandleTaskEdge adds or removes one typed edge out of a task (design d523e74e
// §4.2). The type must be registered (unknown -> INVALID_ARGUMENT); a blocking
// edge that would close a cycle -> EDGE_CYCLE naming the path. Adding a
// blocked_by to a pending task holds it; it cannot un-send an announcement
// already made. Removing an edge settles the task in the same tx, and a hold
// it released is announced here.
func (h *Handlers) HandleTaskEdge(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := h.resolveProject(ctx, req)
	agent := resolveAgent(ctx, req)
	op := req.GetString("op", "")
	typ := strings.TrimSpace(req.GetString("type", ""))
	taskID := req.GetString("task_id", "")
	targetID := req.GetString("target_id", "")
	if taskID == "" || targetID == "" || typ == "" {
		return validationError(CodeInvalidArgument, "task_id, type and target_id are required"), nil
	}
	taskID, err := h.resolveTaskID(taskID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	targetID, err = h.resolveTaskID(targetID, project)
	if err != nil {
		return toolResultError(err.Error()), nil
	}
	resp := map[string]any{"op": op, "task_id": taskID, "type": typ, "target_id": targetID}
	switch op {
	case "add":
		meta := map[string]any{}
		if until := req.GetString("until", ""); until != "" {
			meta["until"] = until
		}
		if err := h.db.AddEdge(project, db.EdgeInput{SrcKind: "task", SrcID: taskID, Type: typ,
			DstKind: "task", DstID: targetID, Metadata: meta, CreatedBy: agent}); err != nil {
			return taskOpError(err, "failed to add edge: %v", err), nil
		}
	case "remove":
		released, err := h.db.RemoveEdge(project, taskID, typ, targetID, agent)
		if err != nil {
			return taskOpError(err, "failed to remove edge: %v", err), nil
		}
		h.announceReleased(project, released)
		resp["released"] = released
	default:
		return validationError(CodeInvalidArgument, fmt.Sprintf("op must be add or remove (got %q)", op)), nil
	}
	if ready, blockers, err := h.db.TaskReadiness(project, taskID); err == nil {
		resp["ready"] = ready
		if len(blockers) > 0 {
			resp["blocked_by"] = blockers
		}
	}
	return h.resultJSONTracked(project, agent, "task_edge", resp)
}
