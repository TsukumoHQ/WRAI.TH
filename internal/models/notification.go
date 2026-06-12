package models

// NotificationRule is a configurable event→action→target rule.
//
//	WHEN {Event} [IF {Match}] THEN {Action} → {Target} [Opts]
//
// Match and Opts are free-form JSON objects:
//   - Match  e.g. {"assignee_is_agent": true}
//   - Opts   e.g. {"ttl": 3600, "priority": "P1", "template": "...", "interval_hours": 8}
type NotificationRule struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Event     string `json:"event"`
	Match     string `json:"match"`  // JSON object
	Action    string `json:"action"` // message | webhook | slack
	Target    string `json:"target"` // agent name | role | "human" | URL
	Opts      string `json:"opts"`   // JSON object
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// NotificationDelivery is a single delivery-log entry recording the outcome of
// firing a rule's action. Capped/pruned to a bounded recent window.
type NotificationDelivery struct {
	ID         string `json:"id"`
	Project    string `json:"project"`
	RuleID     string `json:"rule_id"`
	RuleName   string `json:"rule_name"`
	Event      string `json:"event"`
	Action     string `json:"action"`
	Target     string `json:"target"`
	Outcome    string `json:"outcome"` // ok | failed | dryrun
	StatusCode int    `json:"status_code"`
	Error      string `json:"error"`
	Payload    string `json:"payload"` // JSON snapshot of the sent payload
	CreatedAt  string `json:"created_at"`
}
