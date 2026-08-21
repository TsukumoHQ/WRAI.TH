package models

type Message struct {
	ID             string  `json:"id"`
	From           string  `json:"from"`
	To             string  `json:"to"`
	ReplyTo        *string `json:"reply_to"`
	Type           string  `json:"type"`
	Subject        string  `json:"subject"`
	Content        string  `json:"content"`
	Metadata       string  `json:"metadata"`
	CreatedAt      string  `json:"created_at"`
	ReadAt         *string `json:"read_at"`
	ConversationID *string `json:"conversation_id,omitempty"`
	Project        string  `json:"project"`
	TaskID         *string `json:"task_id,omitempty"`
	Priority       string  `json:"priority"`
	TTLSeconds     int     `json:"ttl_seconds"`
	ExpiredAt      *string `json:"expired_at,omitempty"`
	DeliveryID     *string `json:"delivery_id,omitempty"`
	DeliveryState  *string `json:"delivery_state,omitempty"`
	// ActionRequired is the comms-discipline action tag {ask|do|decide|none}
	// (DEC-relay-comms-discipline-1). Set from the caller or derived server-side
	// at insert. 'none' = no-wake. Populated only where a query SELECTs it
	// explicitly (the wake predicate reads it in a WHERE, not a scan), so it is
	// nil on the existing inbox reads — additive, no scan-lockstep churn.
	ActionRequired *string `json:"action_required,omitempty"`
	// TraceID (trace_id v1) is inherited, never caller-required: a reply takes
	// its parent message's trace_id, a task-announcement message takes its
	// task's (see deriveTraceID, messages.go). Populated at insert time on the
	// two message-creation paths; nil on existing inbox/thread reads (not yet
	// added to their column lists — same additive-not-scanned shape as
	// ActionRequired above).
	TraceID *string `json:"trace_id,omitempty"`
}

// DeliveryStatusRow is one row of the queryable ack state (T4): where a
// message's delivery to a given recipient stands in the lifecycle. Makes
// "what was actually SEEN/acked" auditable — ack_delivery is otherwise
// effectively write-only.
type DeliveryStatusRow struct {
	DeliveryID     string  `json:"delivery_id"`
	MessageID      string  `json:"message_id"`
	ToAgent        string  `json:"to_agent"`
	State          string  `json:"state"`
	CreatedAt      string  `json:"created_at"`
	SurfacedAt     *string `json:"surfaced_at,omitempty"`
	AcknowledgedAt *string `json:"acknowledged_at,omitempty"`
}

// DeadletterRow is one journaled expired-unread message (T6): the durable record
// that a TTL-expired P0/P1 never vanishes silently even after retention GC.
type DeadletterRow struct {
	MessageID string `json:"message_id"`
	ToAgent   string `json:"to_agent"`
	From      string `json:"from"`
	Priority  string `json:"priority"`
	Subject   string `json:"subject"`
	CreatedAt string `json:"created_at"`
	ExpiredAt string `json:"expired_at"`
}
