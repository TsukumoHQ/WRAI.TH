package models

// DayBucket is one day of fleet-wide throughput (mission control pulse chart).
type DayBucket struct {
	Date       string `json:"date"` // YYYY-MM-DD (UTC)
	Done       int    `json:"done"`
	Dispatched int    `json:"dispatched"`
}

type Project struct {
	Name       string `json:"name"`
	PlanetType string `json:"planet_type"`
	CreatedAt  string `json:"created_at"`
}

type ProjectInfo struct {
	Name         string `json:"name"`
	PlanetType   string `json:"planet_type"`
	CreatedAt    string `json:"created_at"`
	AgentCount   int    `json:"agent_count"`
	OnlineCount  int    `json:"online_count"`
	TotalTasks   int    `json:"total_tasks"`
	ActiveTasks  int    `json:"active_tasks"`
	DoneTasks    int    `json:"done_tasks"`
	BlockedTasks int    `json:"blocked_tasks"`
	Tokens24h    int64  `json:"tokens_24h"`
	LastActivity string `json:"last_activity"` // ISO ts of the most recent task movement; "" if none
	ArchivedAt   string `json:"archived_at"`   // RFC3339 when archived; "" if active (DEC-wraith-archive-project-1)
}
