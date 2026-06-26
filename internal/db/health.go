package db

import "time"

// Agent-health thresholds (TSU-53 slice-B).
const (
	healthDeadAfter    = 30 * time.Minute // no last_seen within → dead
	healthRecentWindow = 10 * time.Minute // "spending now" window for working vs idle
)

// AgentHealth is a per-agent liveness + activity snapshot derived from last_seen
// crossed with recent token spend — the data behind the board's health badge.
type AgentHealth struct {
	Agent        string `json:"agent"`
	Status       string `json:"status"` // working | idle | dead
	LastSeen     string `json:"last_seen"`
	IdleSeconds  int64  `json:"idle_seconds"`
	TokensRecent int64  `json:"tokens_recent"` // last healthRecentWindow
	Tokens24h    int64  `json:"tokens_24h"`
}

// classifyHealth derives the badge from idle time × recent spend:
//   - dead:    no activity within healthDeadAfter (the agent's process is gone)
//   - working: seen recently AND spending tokens in the recent window
//   - idle:    seen recently but quiet (waiting / blocked / between turns)
//
// (A "looping" state — fresh + spending + no progress — needs the repeated-tool
// signal and lands in a follow-up; this is the liveness core.)
func classifyHealth(idle time.Duration, tokensRecent int64) string {
	switch {
	case idle > healthDeadAfter:
		return "dead"
	case tokensRecent > 0:
		return "working"
	default:
		return "idle"
	}
}

// parseAgentTime parses a stored timestamp, tolerating both the microsecond
// memory format and RFC3339 (different writers use each).
func parseAgentTime(s string) (time.Time, bool) {
	for _, layout := range []string{memoryTimeFmt, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// GetAgentHealth returns the health snapshot for every agent in a project, newest
// activity first. Tokens use the real per-turn counts when present, else the
// bytes/4 estimate (an honest floor until the transcript hook feeds exact counts).
func (d *DB) GetAgentHealth(project string) ([]AgentHealth, error) {
	agents, err := d.ListAgents(project)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	recentSince := now.Add(-healthRecentWindow).Format(memoryTimeFmt)
	daySince := now.Add(-24 * time.Hour).Format(memoryTimeFmt)

	recent := d.tokensByAgentSince(project, recentSince)
	day := d.tokensByAgentSince(project, daySince)

	out := make([]AgentHealth, 0, len(agents))
	for _, a := range agents {
		h := AgentHealth{
			Agent:        a.Name,
			LastSeen:     a.LastSeen,
			TokensRecent: recent[a.Name],
			Tokens24h:    day[a.Name],
		}
		idle := time.Duration(0)
		if t, ok := parseAgentTime(a.LastSeen); ok {
			idle = now.Sub(t)
			if idle < 0 {
				idle = 0
			}
		} else {
			idle = healthDeadAfter + time.Minute // unparseable → treat as dead
		}
		h.IdleSeconds = int64(idle.Seconds())
		h.Status = classifyHealth(idle, h.TokensRecent)
		out = append(out, h)
	}
	return out, nil
}

// tokensByAgentSince sums each agent's tokens since a time (real counts when
// present, else bytes/4). Returns agent → tokens.
func (d *DB) tokensByAgentSince(project, since string) map[string]int64 {
	res := map[string]int64{}
	rows, err := d.ro().Query(`SELECT agent, `+tokenSum+` FROM token_usage
		WHERE project = ? AND created_at >= ? GROUP BY agent`, project, since)
	if err != nil {
		return res
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var agent string
		var toks int64
		if err := rows.Scan(&agent, &toks); err != nil {
			continue
		}
		res[agent] = toks
	}
	return res
}
