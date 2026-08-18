package models

// Memory represents a persistent piece of agent knowledge.
type Memory struct {
	ID           string  `json:"id"`
	Key          string  `json:"key"`
	Value        string  `json:"value"`
	Tags         string  `json:"tags"`  // JSON array of strings
	Scope        string  `json:"scope"` // "agent", "project", "global"
	Project      string  `json:"project"`
	AgentName    string  `json:"agent_name"`
	Confidence   string  `json:"confidence"` // "stated", "inferred", "observed"
	Version      int     `json:"version"`
	Supersedes   *string `json:"supersedes,omitempty"`    // previous version's memory ID
	ConflictWith *string `json:"conflict_with,omitempty"` // conflicting memory ID
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	ArchivedAt   *string `json:"archived_at,omitempty"`
	ArchivedBy   *string `json:"archived_by,omitempty"`
	Layer        string  `json:"layer"` // "constraints", "behavior", "context"

	// Temporal validity windows (T5). Memories DEGRADE, never silently delete.
	// ValidFrom defaults to created_at; a nil ValidUntil means "no expiry".
	// Status is the EFFECTIVE state as returned by reads: "live" | "stale" |
	// "archived". Time-based staleness (past ValidUntil) is derived at read
	// time, so an expired memory stays stored and searchable, only flagged.
	// ArchivedReason is the tombstone "why" (archived_by is "who", archived_at "when").
	ValidFrom      *string `json:"valid_from,omitempty"`
	ValidUntil     *string `json:"valid_until,omitempty"`
	Status         string  `json:"status"` // "live" | "stale" | "archived"
	ArchivedReason *string `json:"archived_reason,omitempty"`
}
