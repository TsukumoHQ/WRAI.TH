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

	// --- Execution overlay (relay-owned, auto-stamped temporal trail) ---
	ClaimedBy      *string `json:"claimed_by,omitempty"`
	ClaimedAt      *string `json:"claimed_at,omitempty"`
	BlockedPeriods string  `json:"blocked_periods"` // json array of {start,end}
	InReviewAt     *string `json:"in_review_at,omitempty"`
	DoneAt         *string `json:"done_at,omitempty"`

	Subtasks []Task `json:"subtasks,omitempty"`
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
