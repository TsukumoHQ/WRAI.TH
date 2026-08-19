package models

type Task struct {
	ID             string  `json:"id"`
	ProfileSlug    string  `json:"profile_slug"`
	AssignedTo     *string `json:"assigned_to,omitempty"`
	DispatchedBy   string  `json:"dispatched_by"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	Priority       string  `json:"priority"`
	Status         string  `json:"status"`
	Result         *string `json:"result,omitempty"`
	BlockedReason  *string `json:"blocked_reason,omitempty"`
	Project        string  `json:"project"`
	DispatchedAt   string  `json:"dispatched_at"`
	AcceptedAt     *string `json:"accepted_at,omitempty"`
	StartedAt      *string `json:"started_at,omitempty"`
	CompletedAt    *string `json:"completed_at,omitempty"`
	ParentTaskID   *string `json:"parent_task_id,omitempty"`
	AckNotifiedAt  *string `json:"ack_notified_at,omitempty"`
	AckEscalatedAt *string `json:"ack_escalated_at,omitempty"`
	BoardID        *string `json:"board_id,omitempty"`
	ArchivedAt     *string `json:"archived_at,omitempty"`

	// --- Linear zone (read-only, replicated from Linear SSOT) ---
	Source        string  `json:"source"` // 'native' | 'linear'
	LinearIssueID *string `json:"linear_issue_id,omitempty"`
	LinearKey     *string `json:"linear_key,omitempty"` // e.g. SYN-123
	ExternalURL   *string `json:"external_url,omitempty"`
	Points        *int    `json:"points,omitempty"`
	Labels        string  `json:"labels"` // json array
	LinearState   *string `json:"linear_state,omitempty"`
	Assignee      *string `json:"assignee,omitempty"`
	CycleID       *string `json:"cycle_id,omitempty"`
	CycleName     *string `json:"cycle_name,omitempty"`
	CycleStart    *string `json:"cycle_start,omitempty"`
	CycleEnd      *string `json:"cycle_end,omitempty"`
	// LinearProjectID drives project→agent routing for Linear-sourced tasks
	// (e.g. resolving a stale task's assignee from the linear_routing map).
	LinearProjectID *string `json:"linear_project_id,omitempty"`

	// --- Execution overlay (relay-owned, auto-stamped temporal trail) ---
	ClaimedBy      *string `json:"claimed_by,omitempty"`
	ClaimedAt      *string `json:"claimed_at,omitempty"`
	BlockedPeriods string  `json:"blocked_periods"` // json array of {start,end}
	InReviewAt     *string `json:"in_review_at,omitempty"`
	DoneAt         *string `json:"done_at,omitempty"`
	// LastActivityAt bumps on transition/comment/progress-note — the stale-scanner
	// measures idle from here, not from dispatch.
	LastActivityAt *string `json:"last_activity_at,omitempty"`

	// --- Lease zone (T1, atomic ownership) — who currently holds the task and
	// until when. A claim/start/review by the working agent sets the holder and
	// pushes LeaseExpiresAt = now + lease TTL (implicit heartbeat: any forward
	// transition by the holder extends it). complete/block/cancel RELEASE the
	// lease (both cleared). A dead holder's task may be re-claimed only once the
	// lease has expired or the holder is deregistered/inactive; a live holder's
	// lease refuses transfer (TASK_LEASE_HELD). Additive, nullable — an old row
	// with no lease reads as "unheld".
	LeaseHolder    *string `json:"lease_holder,omitempty"`
	LeaseExpiresAt *string `json:"lease_expires_at,omitempty"`
	// LeaseHeartbeatAt is the instant the lease was last extended (the holder's
	// most recent forward transition — the implicit heartbeat). Distinct from
	// LeaseExpiresAt, which is that instant + TTL; kept for audit/observability.
	LeaseHeartbeatAt *string `json:"lease_heartbeat_at,omitempty"`

	// LeaseTransfer is a TRANSIENT, computed field (never persisted, never
	// scanned): when a transition changed the lease holder, the DB layer stamps
	// it so the handler can emit task.lease_transferred (SSE) without re-reading
	// prior state. nil when the transition left the holder unchanged.
	LeaseTransfer *LeaseTransfer `json:"lease_transfer,omitempty"`

	// --- Git zone (review gate) — where the work physically lives, so an
	// external supervisor (e.g. niwa's Q&A gate) can review and merge it.
	// Set by the doer at review_task time; never interpreted by the relay.
	GitBranch   *string `json:"git_branch,omitempty"`
	GitWorktree *string `json:"git_worktree,omitempty"`
	GitTarget   *string `json:"git_target,omitempty"`

	// --- GitHub PR zone (PR-link, DEC-wraith-pr-linking-1) — the linked GitHub
	// PR this task tracks, Linear-style. Set via the link_pr tool (or the niwa
	// gate at PR creation) and synced from GitHub pull_request webhooks. All
	// nullable + additive: a task with no linked PR reads as pr_* = null, so
	// every existing row and older client keeps working. PRRepo is owner/name so
	// a task's PR may live in ANY repo (the founder PRs across repos). PRState is
	// the last observed open|merged|closed.
	PRURL    *string `json:"pr_url,omitempty"`
	PRNumber *int    `json:"pr_number,omitempty"`
	PRState  *string `json:"pr_state,omitempty"`
	PRRepo   *string `json:"pr_repo,omitempty"`

	// --- Run zone (changeset-per-factory-run S1, DEC reuse-parent) — when this
	// task is the PARENT of a multi-agent factory run, it doubles as the run
	// container: IntegrationBranch is the run's shared branch (niwa-set, off the
	// real target), RunState tracks the run lifecycle
	// (open→gating→merging→merged | blocked | amputated). The run = this parent +
	// its subtasks (the agent slices); no separate run_id entity. Both nullable +
	// additive: a task that is not a run reads run_* = null, so every existing row
	// and older client keeps working. A task with RunState set is a container and
	// is NOT claimable as work (guarded in ClaimTask/StartTask) — it groups
	// slices, it is never itself leased by a worker. The relay stores the zone
	// opaquely and stays inbound-only; niwa drives the branch + state transitions.
	IntegrationBranch *string `json:"integration_branch,omitempty"`
	RunState          *string `json:"run_state,omitempty"`

	// --- Typed ticket zone (V-lifecycle, dispatch time) — the deterministic
	// left branch of the V. Goal states intent, AcceptanceCriteria is the
	// per-requirement checklist the review gate verdicts against, Dod is the
	// done bar. Enforced only for projects with require_typed_ticket set
	// (default off — other projects keep dispatching free-form). Additive:
	// empty strings / "[]" mean "no ticket recorded", never break old clients.
	Goal               string `json:"goal"`                // one-line intent
	AcceptanceCriteria string `json:"acceptance_criteria"` // json array of testable items
	Dod                string `json:"dod"`                 // definition of done

	// RefusalNotifiedAt is the anti-spam marker for a Linear issue refused for
	// missing its typed ticket: a REFUSED mirror row (Status "refused") carries
	// it, stamped when the one loud comment was posted so no path re-comments.
	// Cleared when the issue becomes conforming, so a later regression re-notifies
	// once. Set only on Linear-sourced refused rows.
	RefusalNotifiedAt *string `json:"refusal_notified_at,omitempty"`

	Subtasks []Task `json:"subtasks,omitempty"`
}

// LeaseTransfer describes a change of a task's lease holder, carried on the
// task.lease_transferred event (SSE) and the audit trail. Reason is one of
// "expired" | "deregistered" (dead-holder re-claim), or "voluntary" (the holder
// released via complete/block/cancel, or an orchestrator reassign). To is empty
// on a pure release (holder → none).
type LeaseTransfer struct {
	From   string `json:"from"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason"`
}

// AuditEntry is one logged orchestrator/agent action against a resource — the
// "why" trail behind every consequential move on the board.
type AuditEntry struct {
	ID           string `json:"id"`
	Project      string `json:"project"`
	Actor        string `json:"actor"`         // who did it ("user" for the orchestrator)
	Action       string `json:"action"`        // transition | force_transition | set_dependencies | reassign
	ResourceType string `json:"resource_type"` // "task"
	ResourceID   string `json:"resource_id"`
	Summary      string `json:"summary"`           // one-line human description
	Details      string `json:"details,omitempty"` // optional json blob (old/new)
	Reason       string `json:"reason,omitempty"`
	CreatedAt    string `json:"created_at"`
}

type Board struct {
	ID          string  `json:"id"`
	Project     string  `json:"project"`
	Name        string  `json:"name"`
	Slug        string  `json:"slug"`
	Description string  `json:"description"`
	CreatedBy   string  `json:"created_by"`
	CreatedAt   string  `json:"created_at"`
	ArchivedAt  *string `json:"archived_at,omitempty"`
}
