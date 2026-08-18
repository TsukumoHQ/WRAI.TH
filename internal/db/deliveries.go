package db

import (
	"agent-relay/internal/models"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CreateDeliveries creates delivery records for a message to the specified recipients.
func (d *DB) CreateDeliveries(messageID, project string, recipients []string) error {
	if len(recipients) == 0 {
		return nil
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	// One transaction for the whole fan-out: N recipients = 1 write-lock acquire
	// + 1 fsync instead of N. Matters on the hot send/broadcast path.
	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("create deliveries: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		"INSERT INTO deliveries (id, message_id, to_agent, state, sequence_number, created_at, project) VALUES (?, ?, ?, 'queued', ?, ?, ?)",
	)
	if err != nil {
		return fmt.Errorf("create deliveries: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for i, agent := range recipients {
		if _, err := stmt.Exec(uuid.New().String(), messageID, agent, i, now, project); err != nil {
			return fmt.Errorf("create delivery for %s: %w", agent, err)
		}
	}
	return tx.Commit()
}

// MarkDeliveriesSurfaced transitions the given delivery IDs from 'queued' to 'surfaced'.
// No-op for IDs not currently queued. Used by HandleGetInbox to surface only the
// deliveries that survived budget pruning, so dropped messages stay available for
// the next poll.
func (d *DB) MarkDeliveriesSurfaced(ids []string) {
	if len(ids) == 0 {
		return
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	for _, id := range ids {
		_, _ = d.conn.Exec("UPDATE deliveries SET state = 'surfaced', surfaced_at = ? WHERE id = ? AND state = 'queued'", now, id)
	}
}

// GetInboxViaDeliveries returns messages for an agent using the deliveries table.
//
// It is a NON-DESTRUCTIVE peek: it marks returned 'queued' deliveries as
// 'surfaced' for analytics/first-read tracking, but surfacing no longer hides a
// message from the next unread poll. "Unread" means NOT acknowledged (queued OR
// surfaced); a message only leaves the unread view via an explicit mark_read /
// ack_delivery. Previously unread filtered state='queued' and the fetch's
// auto-surface flipped it out of unread after one read — so the single unread
// view (truncated by the handler) was the only chance to see a message, and its
// full content was silently lost (TSU-73).
func (d *DB) GetInboxViaDeliveries(project, agentName string, unreadOnly bool, limit int, filters ...InboxFilter) ([]models.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	var f InboxFilter
	if len(filters) > 0 {
		f = filters[0]
	}

	query := `
		SELECT m.id, m.from_agent, m.to_agent, m.reply_to, m.type, m.subject, m.content, m.metadata,
		       m.created_at, m.read_at, m.conversation_id, m.project, m.task_id, m.priority, m.ttl_seconds, m.expired_at,
		       d.id, d.state
		FROM deliveries d
		JOIN messages m ON d.message_id = m.id
		WHERE d.project = ? AND d.to_agent = ?
		  AND d.state != 'expired'
		  AND m.expired_at IS NULL
		  AND (m.ttl_seconds = 0 OR datetime(m.created_at, '+' || m.ttl_seconds || ' seconds') > datetime('now'))
	`
	args := []any{project, agentName}

	if unreadOnly {
		// Unread = not yet acknowledged. A non-destructive peek: surfaced
		// messages stay visible until an explicit mark_read / ack_delivery.
		query += " AND d.state IN ('queued', 'surfaced')"
	}
	if f.MinPriority != "" {
		query += " AND m.priority <= ?"
		args = append(args, f.MinPriority)
	}
	if f.From != "" {
		query += " AND m.from_agent = ?"
		args = append(args, f.From)
	}
	if f.Since != "" {
		query += " AND m.created_at >= ?"
		args = append(args, f.Since)
	}
	if f.ExcludeBroadcasts {
		query += " AND m.to_agent != '*'"
	}

	query += " ORDER BY m.priority ASC, m.created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get inbox via deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []models.Message
	var deliveryIDs []string
	for rows.Next() {
		var m models.Message
		var deliveryID, deliveryState string
		if err := rows.Scan(
			&m.ID, &m.From, &m.To, &m.ReplyTo, &m.Type, &m.Subject, &m.Content, &m.Metadata,
			&m.CreatedAt, &m.ReadAt, &m.ConversationID, &m.Project, &m.TaskID, &m.Priority, &m.TTLSeconds, &m.ExpiredAt,
			&deliveryID, &deliveryState,
		); err != nil {
			return nil, fmt.Errorf("scan delivery message: %w", err)
		}
		m.DeliveryID = &deliveryID
		m.DeliveryState = &deliveryState
		messages = append(messages, m)
		if deliveryState == "queued" {
			deliveryIDs = append(deliveryIDs, deliveryID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Mark queued deliveries as surfaced.
	// Callers applying post-fetch filtering (budget pruning) should use
	// FetchInboxDeliveries + MarkDeliveriesSurfaced instead so dropped
	// deliveries stay available for the next poll.
	d.MarkDeliveriesSurfaced(deliveryIDs)

	return messages, nil
}

// FetchInboxDeliveries returns messages without marking them surfaced.
// Use with MarkDeliveriesSurfaced once the caller has decided which messages
// are actually being delivered (e.g. after budget pruning).
func (d *DB) FetchInboxDeliveries(project, agentName string, unreadOnly bool, limit int, filters ...InboxFilter) ([]models.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	var f InboxFilter
	if len(filters) > 0 {
		f = filters[0]
	}

	query := `
		SELECT m.id, m.from_agent, m.to_agent, m.reply_to, m.type, m.subject, m.content, m.metadata,
		       m.created_at, m.read_at, m.conversation_id, m.project, m.task_id, m.priority, m.ttl_seconds, m.expired_at,
		       d.id, d.state
		FROM deliveries d
		JOIN messages m ON d.message_id = m.id
		WHERE d.project = ? AND d.to_agent = ?
		  AND d.state != 'expired'
		  AND m.expired_at IS NULL
		  AND (m.ttl_seconds = 0 OR datetime(m.created_at, '+' || m.ttl_seconds || ' seconds') > datetime('now'))
	`
	args := []any{project, agentName}

	if unreadOnly {
		// Unread = not yet acknowledged (see GetInboxViaDeliveries).
		query += " AND d.state IN ('queued', 'surfaced')"
	}
	if f.MinPriority != "" {
		query += " AND m.priority <= ?"
		args = append(args, f.MinPriority)
	}
	if f.From != "" {
		query += " AND m.from_agent = ?"
		args = append(args, f.From)
	}
	if f.Since != "" {
		query += " AND m.created_at >= ?"
		args = append(args, f.Since)
	}
	if f.ExcludeBroadcasts {
		query += " AND m.to_agent != '*'"
	}

	query += " ORDER BY m.priority ASC, m.created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch inbox deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []models.Message
	for rows.Next() {
		var m models.Message
		var deliveryID, deliveryState string
		if err := rows.Scan(
			&m.ID, &m.From, &m.To, &m.ReplyTo, &m.Type, &m.Subject, &m.Content, &m.Metadata,
			&m.CreatedAt, &m.ReadAt, &m.ConversationID, &m.Project, &m.TaskID, &m.Priority, &m.TTLSeconds, &m.ExpiredAt,
			&deliveryID, &deliveryState,
		); err != nil {
			return nil, fmt.Errorf("scan delivery message: %w", err)
		}
		m.DeliveryID = &deliveryID
		m.DeliveryState = &deliveryState
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// AcknowledgeDelivery marks a delivery as acknowledged.
func (d *DB) AcknowledgeDelivery(deliveryID string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	_, err := d.conn.Exec(
		"UPDATE deliveries SET state = 'acknowledged', acknowledged_at = ? WHERE id = ? AND state IN ('queued', 'surfaced')",
		now, deliveryID,
	)
	return err
}

// DeliveryStatus returns the queryable ack state (T4) filtered by message_id OR
// by recipient agent (at least one required). Read-only — makes surfaced/acked
// auditable. Rows ordered oldest-first.
func (d *DB) DeliveryStatus(project, messageID, agent string) ([]models.DeliveryStatusRow, error) {
	var where string
	var args []any
	switch {
	case messageID != "":
		where = "message_id = ? AND project = ?"
		args = []any{messageID, project}
	case agent != "":
		where = "to_agent = ? AND project = ?"
		args = []any{strings.ToLower(strings.TrimSpace(agent)), project}
	default:
		return nil, fmt.Errorf("delivery_status requires message_id or agent")
	}
	rows, err := d.ro().Query(
		"SELECT id, message_id, to_agent, state, created_at, surfaced_at, acknowledged_at "+
			"FROM deliveries WHERE "+where+" ORDER BY created_at, sequence_number", args...,
	)
	if err != nil {
		return nil, fmt.Errorf("delivery_status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []models.DeliveryStatusRow
	for rows.Next() {
		var r models.DeliveryStatusRow
		if err := rows.Scan(&r.DeliveryID, &r.MessageID, &r.ToAgent, &r.State, &r.CreatedAt, &r.SurfacedAt, &r.AcknowledgedAt); err != nil {
			return nil, fmt.Errorf("scan delivery_status: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CrossProjectUnread rolls up an agent's still-unread deliveries in EVERY
// project except excludeProject, as {project: {unread, p0}} (T4). Counts only —
// no bodies — so it respects the session_context payload ceiling (WRAITH-1). A
// single grouped query; projects with zero unread are omitted.
func (d *DB) CrossProjectUnread(agent, excludeProject string) (map[string]map[string]int, error) {
	rows, err := d.ro().Query(`
		SELECT d.project, COUNT(*) AS unread,
		       COALESCE(SUM(CASE WHEN m.priority = 'P0' THEN 1 ELSE 0 END), 0) AS p0
		FROM deliveries d JOIN messages m ON d.message_id = m.id
		WHERE d.to_agent = ? AND d.project <> ? AND d.state IN ('queued', 'surfaced')
		GROUP BY d.project`,
		strings.ToLower(strings.TrimSpace(agent)), excludeProject,
	)
	if err != nil {
		return nil, fmt.Errorf("cross_project_unread: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]int{}
	for rows.Next() {
		var proj string
		var unread, p0 int
		if err := rows.Scan(&proj, &unread, &p0); err != nil {
			return nil, fmt.Errorf("scan cross_project_unread: %w", err)
		}
		out[proj] = map[string]int{"unread": unread, "p0": p0}
	}
	return out, rows.Err()
}

// DeliveryIDsForAgent returns {message_id: delivery_id} for the given agent's
// own deliveries among messageIDs (T4) — lets a message-returning handler attach
// the caller's delivery_id without threading it through every query. A message
// with no delivery to this agent is simply absent from the map.
func (d *DB) DeliveryIDsForAgent(project, agent string, messageIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(messageIDs)), ",")
	args := make([]any, 0, len(messageIDs)+2)
	args = append(args, strings.ToLower(strings.TrimSpace(agent)), project)
	for _, id := range messageIDs {
		args = append(args, id)
	}
	rows, err := d.ro().Query(
		"SELECT message_id, id FROM deliveries WHERE to_agent = ? AND project = ? AND message_id IN ("+placeholders+")",
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("delivery_ids_for_agent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var mid, did string
		if err := rows.Scan(&mid, &did); err != nil {
			return nil, fmt.Errorf("scan delivery_ids: %w", err)
		}
		out[mid] = did
	}
	return out, rows.Err()
}

// AcknowledgeDeliveryByMessage finds a delivery by message_id + agent and acknowledges it.
// Used for backward compat with mark_read.
func (d *DB) AcknowledgeDeliveryByMessage(messageID, agentName, project string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	_, err := d.conn.Exec(
		"UPDATE deliveries SET state = 'acknowledged', acknowledged_at = ? WHERE message_id = ? AND to_agent = ? AND project = ? AND state IN ('queued', 'surfaced')",
		now, messageID, agentName, project,
	)
	return err
}

// AcknowledgeConversationDeliveries acks every still-open delivery to an agent
// for messages in a conversation. mark_read(conversation_id) writes
// conversation_reads but historically never touched deliveries, so a
// conversation's messages stayed queued/surfaced and kept re-surfacing until an
// explicit ack_delivery (WRAITH-2). Acking here makes mark_read a real clear for
// conversation messages, matching what the message_ids branch already does.
func (d *DB) AcknowledgeConversationDeliveries(conversationID, agentName, project string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	_, err := d.conn.Exec(
		`UPDATE deliveries SET state = 'acknowledged', acknowledged_at = ?
		 WHERE to_agent = ? AND project = ? AND state IN ('queued', 'surfaced')
		   AND message_id IN (SELECT id FROM messages WHERE conversation_id = ?)`,
		now, agentName, project, conversationID,
	)
	return err
}

// ExpireDeliveries marks deliveries for expired messages.
func (d *DB) ExpireDeliveries() (int, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	result, err := d.conn.Exec(
		`UPDATE deliveries SET state = 'expired', expired_at = ?
		 WHERE state IN ('queued', 'surfaced')
		   AND message_id IN (SELECT id FROM messages WHERE expired_at IS NOT NULL)`,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("expire deliveries: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// HasDeliveries returns true if the deliveries table has any rows.
func (d *DB) HasDeliveries() bool {
	var count int
	_ = d.ro().QueryRow("SELECT COUNT(*) FROM deliveries LIMIT 1").Scan(&count)
	return count > 0
}

// ResolveRecipients determines the actual recipient agents for a message.
func (d *DB) ResolveRecipients(project, to, from string, conversationID *string) ([]string, error) {
	if conversationID != nil {
		// Conversation: all members except sender
		members, err := d.GetConversationMembers(*conversationID)
		if err != nil {
			return nil, err
		}
		var recipients []string
		for _, m := range members {
			if m.AgentName != from {
				recipients = append(recipients, m.AgentName)
			}
		}
		return recipients, nil
	}

	if to == "*" {
		// Broadcast: all active agents in project except sender
		agents, err := d.ListAgents(project)
		if err != nil {
			return nil, err
		}
		var recipients []string
		for _, a := range agents {
			if a.Name != from {
				recipients = append(recipients, a.Name)
			}
		}
		return recipients, nil
	}

	// Direct: single recipient
	return []string{to}, nil
}
