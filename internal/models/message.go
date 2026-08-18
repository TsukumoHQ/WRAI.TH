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
