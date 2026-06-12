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
}
