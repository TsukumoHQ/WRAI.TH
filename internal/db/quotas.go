package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"
)

// SetAgentQuota creates or updates quota limits for an agent.
func (d *DB) SetAgentQuota(project, agentName string, maxTokens, maxMessages, maxTasks, maxSpawns int64) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec(`INSERT INTO agent_quotas (project, agent_name, max_tokens_per_day, max_messages_per_hour, max_tasks_per_hour, max_spawns_per_hour, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project, agent_name) DO UPDATE SET
			max_tokens_per_day=excluded.max_tokens_per_day,
			max_messages_per_hour=excluded.max_messages_per_hour,
			max_tasks_per_hour=excluded.max_tasks_per_hour,
			max_spawns_per_hour=excluded.max_spawns_per_hour,
			updated_at=excluded.updated_at`,
		project, agentName, maxTokens, maxMessages, maxTasks, maxSpawns, now, now)
	return err
}

// GetAgentQuota returns the quota for an agent, or nil if none set.
func (d *DB) GetAgentQuota(project, agentName string) (*models.AgentQuota, error) {
	var q models.AgentQuota
	err := d.ro().QueryRow(`SELECT project, agent_name, max_tokens_per_day, max_messages_per_hour,
		max_tasks_per_hour, max_spawns_per_hour, created_at, updated_at
		FROM agent_quotas WHERE project = ? AND agent_name = ?`, project, agentName).Scan(
		&q.Project, &q.AgentName, &q.MaxTokensPerDay, &q.MaxMessagesPerHour,
		&q.MaxTasksPerHour, &q.MaxSpawnsPerHour, &q.CreatedAt, &q.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// ListAgentQuotas returns all quotas for a project.
func (d *DB) ListAgentQuotas(project string) ([]models.AgentQuota, error) {
	rows, err := d.ro().Query(`SELECT project, agent_name, max_tokens_per_day, max_messages_per_hour,
		max_tasks_per_hour, max_spawns_per_hour, created_at, updated_at
		FROM agent_quotas WHERE project = ? ORDER BY agent_name`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.AgentQuota
	for rows.Next() {
		var q models.AgentQuota
		if err := rows.Scan(&q.Project, &q.AgentName, &q.MaxTokensPerDay, &q.MaxMessagesPerHour,
			&q.MaxTasksPerHour, &q.MaxSpawnsPerHour, &q.CreatedAt, &q.UpdatedAt); err != nil {
			continue
		}
		result = append(result, q)
	}
	return result, rows.Err()
}

// DeleteQuota removes a quota for an agent.
func (d *DB) DeleteQuota(project, agentName string) error {
	_, err := d.conn.Exec("DELETE FROM agent_quotas WHERE project = ? AND agent_name = ?", project, agentName)
	return err
}

// CheckQuota checks if an agent has exceeded a specific quota type.
// Returns (allowed, used, limit). If no quota is set, returns (true, 0, 0).
func (d *DB) CheckQuota(project, agentName, quotaType string) (allowed bool, used, limit int64) {
	q, err := d.GetAgentQuota(project, agentName)
	if err != nil || q == nil {
		return true, 0, 0 // no quota = no limit
	}

	switch quotaType {
	case "tokens":
		if q.MaxTokensPerDay <= 0 {
			return true, 0, 0
		}
		used = d.countTokenUsage24h(project, agentName)
		return used < q.MaxTokensPerDay, used, q.MaxTokensPerDay

	case "messages":
		if q.MaxMessagesPerHour <= 0 {
			return true, 0, 0
		}
		used = d.countMessages1h(project, agentName)
		return used < q.MaxMessagesPerHour, used, q.MaxMessagesPerHour

	case "tasks":
		if q.MaxTasksPerHour <= 0 {
			return true, 0, 0
		}
		used = d.countTasks1h(project, agentName)
		return used < q.MaxTasksPerHour, used, q.MaxTasksPerHour

	case "spawns":
		if q.MaxSpawnsPerHour <= 0 {
			return true, 0, 0
		}
		used = d.countSpawns1h(project, agentName)
		return used < q.MaxSpawnsPerHour, used, q.MaxSpawnsPerHour

	default:
		return true, 0, 0
	}
}

// GetQuotaUsage returns current usage for an agent across all quota types.
func (d *DB) GetQuotaUsage(project, agentName string) (*models.QuotaUsage, error) {
	q, err := d.GetAgentQuota(project, agentName)
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &models.AgentQuota{Project: project, AgentName: agentName}
	}

	return &models.QuotaUsage{
		AgentQuota:     *q,
		TokensUsed24h:  d.countTokenUsage24h(project, agentName),
		MessagesUsed1h: d.countMessages1h(project, agentName),
		TasksUsed1h:    d.countTasks1h(project, agentName),
		SpawnsUsed1h:   d.countSpawns1h(project, agentName),
	}, nil
}

func (d *DB) countTokenUsage24h(project, agent string) int64 {
	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(memoryTimeFmt)
	var count int64
	// Use the real per-turn token count when present, else the bytes/4 estimate
	// (tokenSum) — a per-day TOKEN quota must measure tokens, not raw payload bytes.
	_ = d.ro().QueryRow(`SELECT COALESCE(`+tokenSum+`, 0) FROM token_usage WHERE project = ? AND agent = ? AND created_at > ?`,
		project, agent, cutoff).Scan(&count)
	return count
}

func (d *DB) countMessages1h(project, agent string) int64 {
	cutoff := time.Now().UTC().Add(-1 * time.Hour).Format(memoryTimeFmt)
	var count int64
	_ = d.ro().QueryRow(`SELECT COUNT(*) FROM messages WHERE project = ? AND from_agent = ? AND created_at > ?`,
		project, agent, cutoff).Scan(&count)
	return count
}

func (d *DB) countTasks1h(project, agent string) int64 {
	cutoff := time.Now().UTC().Add(-1 * time.Hour).Format(memoryTimeFmt)
	var count int64
	_ = d.ro().QueryRow(`SELECT COUNT(*) FROM tasks WHERE project = ? AND dispatched_by = ? AND dispatched_at > ?`,
		project, agent, cutoff).Scan(&count)
	return count
}

func (d *DB) countSpawns1h(project, agent string) int64 {
	cutoff := time.Now().UTC().Add(-1 * time.Hour).Format(memoryTimeFmt)
	var count int64
	_ = d.ro().QueryRow(`SELECT COUNT(*) FROM spawn_children WHERE project = ? AND parent_agent = ? AND started_at > ?`,
		project, agent, cutoff).Scan(&count)
	return count
}

// checkQuotaError returns a formatted error string if quota exceeded, empty string if OK.
func (d *DB) CheckQuotaError(project, agent, quotaType string) string {
	allowed, used, limit := d.CheckQuota(project, agent, quotaType)
	if allowed {
		return ""
	}
	return fmt.Sprintf("quota exceeded: %s %d/%d for agent '%s'", quotaType, used, limit, agent)
}
