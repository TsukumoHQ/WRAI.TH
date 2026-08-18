package models

type Agent struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Role            string  `json:"role"`
	Description     string  `json:"description"`
	RegisteredAt    string  `json:"registered_at"`
	LastSeen        string  `json:"last_seen"`
	Project         string  `json:"project"`
	ReportsTo       *string `json:"reports_to,omitempty"`
	ProfileSlug     *string `json:"profile_slug,omitempty"`
	Status          string  `json:"status"`
	DeactivatedAt   *string `json:"deactivated_at,omitempty"`
	IsExecutive     bool    `json:"is_executive"`
	SessionID       *string `json:"session_id,omitempty"`
	InterestTags    string  `json:"interest_tags"`
	MaxContextBytes int     `json:"max_context_bytes"`
	AvatarURL       *string `json:"avatar_url,omitempty"`
	// IsService marks a system/daemon identity (monitoring, QA) that must be able
	// to post even when every worker is dead — always eligible to send, exempt
	// from the sender-liveness gate (still subject to auth). Preserved across
	// respawns like IsExecutive. See db.SenderEligibility (T2).
	IsService bool `json:"is_service"`
}
