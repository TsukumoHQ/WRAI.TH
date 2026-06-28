package db

import "time"

// Stats holds aggregate relay statistics.
type Stats struct {
	Agents      int
	Messages    int
	Unread      int
	Threads     int
	OldestAgent string // RFC3339 — earliest registered_at (proxy for uptime)
}

// GetStats returns aggregate counts from the database for a project.
func (d *DB) GetStats(project string) (*Stats, error) {
	s := &Stats{}

	err := d.ro().QueryRow("SELECT COUNT(*) FROM agents WHERE project = ?", project).Scan(&s.Agents)
	if err != nil {
		return nil, err
	}

	err = d.ro().QueryRow("SELECT COUNT(*) FROM messages WHERE project = ?", project).Scan(&s.Messages)
	if err != nil {
		return nil, err
	}

	err = d.ro().QueryRow("SELECT COUNT(*) FROM messages WHERE read_at IS NULL AND project = ?", project).Scan(&s.Unread)
	if err != nil {
		return nil, err
	}

	err = d.ro().QueryRow(`
		SELECT COUNT(DISTINCT CASE WHEN reply_to IS NULL THEN id ELSE reply_to END)
		FROM messages
		WHERE project = ?
	`, project).Scan(&s.Threads)
	if err != nil {
		return nil, err
	}

	// Oldest agent registration as uptime proxy.
	var oldest *string
	err = d.ro().QueryRow("SELECT MIN(registered_at) FROM agents WHERE project = ?", project).Scan(&oldest)
	if err == nil && oldest != nil {
		s.OldestAgent = *oldest
	}

	return s, nil
}

// GetGlobalStats returns aggregate counts across all projects (for CLI status).
func (d *DB) GetGlobalStats() (*Stats, error) {
	s := &Stats{}

	err := d.ro().QueryRow("SELECT COUNT(*) FROM agents").Scan(&s.Agents)
	if err != nil {
		return nil, err
	}

	err = d.ro().QueryRow("SELECT COUNT(*) FROM messages").Scan(&s.Messages)
	if err != nil {
		return nil, err
	}

	err = d.ro().QueryRow("SELECT COUNT(*) FROM messages WHERE read_at IS NULL").Scan(&s.Unread)
	if err != nil {
		return nil, err
	}

	err = d.ro().QueryRow(`
		SELECT COUNT(DISTINCT CASE WHEN reply_to IS NULL THEN id ELSE reply_to END)
		FROM messages
	`).Scan(&s.Threads)
	if err != nil {
		return nil, err
	}

	var oldest *string
	err = d.ro().QueryRow("SELECT MIN(registered_at) FROM agents").Scan(&oldest)
	if err == nil && oldest != nil {
		s.OldestAgent = *oldest
	}

	return s, nil
}

// AgentCount returns just the number of agents (for lightweight status check).
func (d *DB) AgentCount() (int, error) {
	var n int
	err := d.ro().QueryRow("SELECT COUNT(*) FROM agents").Scan(&n)
	return n, err
}

// UnreadCount returns the total number of unread messages across all agents.
func (d *DB) UnreadCount() (int, error) {
	var n int
	err := d.ro().QueryRow("SELECT COUNT(*) FROM messages WHERE read_at IS NULL").Scan(&n)
	return n, err
}

// OpsMetrics is a lightweight fleet-health snapshot for the ops/metrics endpoint
// (TSU-141). All counts come from the read pool; a monitor scrapes this to track
// fleet activity, message throughput, GC pressure, and table growth.
type OpsMetrics struct {
	AgentsActive             int64 `json:"agents_active"`
	AgentsTotal              int64 `json:"agents_total"`
	MessagesTotal            int64 `json:"messages_total"`
	MessagesLastMinute       int64 `json:"messages_last_minute"`
	MessagesLastHour         int64 `json:"messages_last_hour"`
	MessagesExpiredPendingGC int64 `json:"messages_expired_pending_gc"`
	DeliveriesUnacked        int64 `json:"deliveries_unacked"`
	TasksOpen                int64 `json:"tasks_open"`
	AuditLogRows             int64 `json:"audit_log_rows"`
	MemoriesRows             int64 `json:"memories_rows"`
}

// MetricsSnapshot returns a one-shot OpsMetrics. Best-effort: a failing count
// leaves its field at zero rather than failing the whole scrape, so a monitoring
// endpoint never 500s on one bad query.
func (d *DB) MetricsSnapshot() OpsMetrics {
	var m OpsMetrics
	ro := d.ro()
	scan := func(dst *int64, query string, args ...any) {
		_ = ro.QueryRow(query, args...).Scan(dst)
	}
	now := time.Now().UTC()
	minAgo := now.Add(-time.Minute).Format(memoryTimeFmt)
	hourAgo := now.Add(-time.Hour).Format(memoryTimeFmt)

	scan(&m.AgentsTotal, `SELECT COUNT(*) FROM agents`)
	scan(&m.AgentsActive, `SELECT COUNT(*) FROM agents WHERE status = 'active'`)
	scan(&m.MessagesTotal, `SELECT COUNT(*) FROM messages`)
	scan(&m.MessagesLastMinute, `SELECT COUNT(*) FROM messages WHERE created_at > ?`, minAgo)
	scan(&m.MessagesLastHour, `SELECT COUNT(*) FROM messages WHERE created_at > ?`, hourAgo)
	scan(&m.MessagesExpiredPendingGC, `SELECT COUNT(*) FROM messages WHERE expired_at IS NOT NULL`)
	scan(&m.DeliveriesUnacked, `SELECT COUNT(*) FROM deliveries WHERE state IN ('queued', 'surfaced')`)
	scan(&m.TasksOpen, `SELECT COUNT(*) FROM tasks WHERE status NOT IN ('done', 'cancelled')`)
	scan(&m.AuditLogRows, `SELECT COUNT(*) FROM audit_log`)
	scan(&m.MemoriesRows, `SELECT COUNT(*) FROM memories`)
	return m
}
