package db

import (
	"agent-relay/internal/models"
	"agent-relay/internal/normalize"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (d *DB) InsertMessage(project, from, to, msgType, subject, content, metadata, priority string, ttlSeconds int, replyTo, conversationID *string) (*models.Message, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	if priority == "" {
		priority = "P2"
	}
	if ttlSeconds < 0 {
		ttlSeconds = 14400
	}

	msg := &models.Message{
		ID:             uuid.New().String(),
		From:           from,
		To:             to,
		ReplyTo:        replyTo,
		Type:           msgType,
		Subject:        subject,
		Content:        normalize.JSONKeys(content),
		Metadata:       normalize.JSONKeys(metadata),
		CreatedAt:      now,
		ConversationID: conversationID,
		Project:        project,
		Priority:       priority,
		TTLSeconds:     ttlSeconds,
	}

	_, err := d.conn.Exec(
		"INSERT INTO messages (id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, conversation_id, project, priority, ttl_seconds) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		msg.ID, msg.From, msg.To, msg.ReplyTo, msg.Type, msg.Subject, msg.Content, msg.Metadata, msg.CreatedAt, msg.ConversationID, msg.Project, msg.Priority, msg.TTLSeconds,
	)
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	return msg, nil
}

// InboxFilter holds optional filtering parameters for get_inbox.
type InboxFilter struct {
	MinPriority       string // e.g. "P1" — only messages with priority <= this
	From              string // filter by sender
	Since             string // ISO timestamp — only messages after this time
	ExcludeBroadcasts bool   // exclude broadcast messages (to_agent = '*')
}

func (d *DB) GetInbox(project, agentName string, unreadOnly bool, limit int, filters ...InboxFilter) ([]models.Message, error) {
	var f InboxFilter
	if len(filters) > 0 {
		f = filters[0]
	}
	// Use delivery-based inbox when deliveries exist
	if d.HasDeliveries() {
		return d.GetInboxViaDeliveries(project, agentName, unreadOnly, limit, f)
	}
	return d.getInboxLegacy(project, agentName, unreadOnly, limit, f)
}

// getInboxLegacy is the original inbox query for DBs without deliveries.
func (d *DB) getInboxLegacy(project, agentName string, unreadOnly bool, limit int, f InboxFilter) ([]models.Message, error) {
	if limit <= 0 {
		limit = 50
	}

	broadcastClause := "(m.to_agent = '*' AND m.from_agent != ?)"
	if f.ExcludeBroadcasts {
		broadcastClause = "0" // never match broadcasts
	}

	query := fmt.Sprintf(`
		SELECT m.id, m.from_agent, m.to_agent, m.reply_to, m.type, m.subject, m.content, m.metadata, m.created_at, m.read_at, m.conversation_id, m.project, m.task_id, m.priority, m.ttl_seconds, m.expired_at
		FROM messages m
		WHERE m.project = ?
			AND (
				(m.conversation_id IS NULL AND (m.to_agent = ? OR %s))
				OR (m.conversation_id IS NOT NULL AND m.conversation_id IN (
					SELECT conversation_id FROM conversation_members
					WHERE agent_name = ? AND left_at IS NULL
				) AND m.from_agent != ?)
			)
			AND m.expired_at IS NULL
	`, broadcastClause)
	args := []any{project, agentName}
	if !f.ExcludeBroadcasts {
		args = append(args, agentName)
	}
	args = append(args, agentName, agentName)

	if unreadOnly {
		query += ` AND NOT EXISTS (
			SELECT 1 FROM message_reads mr WHERE mr.message_id = m.id AND mr.agent_name = ?
		)`
		args = append(args, agentName)
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

	query += " ORDER BY m.priority ASC, m.created_at DESC LIMIT ?"
	args = append(args, limit)

	return d.queryMessages(query, args...)
}

func (d *DB) GetThread(messageID string) ([]models.Message, error) {
	rootID := messageID
	// Walk up to the root, bounded so a cyclic/self-referential reply_to chain
	// (buggy or malicious) can't spin forever.
	for i := 0; i < 200; i++ {
		var replyTo *string
		err := d.ro().QueryRow("SELECT reply_to FROM messages WHERE id = ?", rootID).Scan(&replyTo)
		if err != nil {
			break
		}
		if replyTo == nil {
			break
		}
		rootID = *replyTo
	}

	// Recursive descent is depth-bounded (depth < 200) and row-capped (LIMIT 200)
	// so a pathological reply chain can't OOM the relay.
	query := `
		WITH RECURSIVE thread(id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at, depth) AS (
			SELECT id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at, 0
			FROM messages WHERE id = ?
			UNION ALL
			SELECT m.id, m.from_agent, m.to_agent, m.reply_to, m.type, m.subject, m.content, m.metadata, m.created_at, m.read_at, m.conversation_id, m.project, m.task_id, m.priority, m.ttl_seconds, m.expired_at, t.depth + 1
			FROM messages m
			JOIN thread t ON m.reply_to = t.id
			WHERE t.depth < 200
		)
		SELECT id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at
		FROM thread ORDER BY created_at ASC LIMIT 200
	`

	return d.queryMessages(query, rootID)
}

func (d *DB) MarkRead(messageIDs []string, agentName, project string) (int, error) {
	if len(messageIDs) == 0 {
		return 0, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	count := 0

	for _, id := range messageIDs {
		result, err := d.conn.Exec(
			"INSERT OR IGNORE INTO message_reads (message_id, agent_name, project, read_at) VALUES (?, ?, ?, ?)",
			id, agentName, project, now,
		)
		if err != nil {
			return count, fmt.Errorf("mark read: %w", err)
		}
		n, _ := result.RowsAffected()
		count += int(n)
		// Also acknowledge the delivery (backward compat)
		_ = d.AcknowledgeDeliveryByMessage(id, agentName, project)
	}

	// Also update conversation_reads for any conversation messages
	convPlaceholders := ""
	convArgs := make([]any, 0, len(messageIDs))
	for i, id := range messageIDs {
		if i > 0 {
			convPlaceholders += ","
		}
		convPlaceholders += "?"
		convArgs = append(convArgs, id)
	}
	convRows, err := d.conn.Query(
		fmt.Sprintf("SELECT DISTINCT conversation_id FROM messages WHERE id IN (%s) AND conversation_id IS NOT NULL", convPlaceholders),
		convArgs...,
	)
	if err == nil {
		var convIDs []string
		for convRows.Next() {
			var convID string
			if err := convRows.Scan(&convID); err == nil {
				convIDs = append(convIDs, convID)
			}
		}
		_ = convRows.Close()
		for _, convID := range convIDs {
			_ = d.MarkConversationRead(convID, agentName)
		}
	}

	return count, nil
}

func (d *DB) GetMessage(id string) (*models.Message, error) {
	msgs, err := d.queryMessages(
		"SELECT id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at FROM messages WHERE id = ?",
		id,
	)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return &msgs[0], nil
}

// FindMessageByPrefix resolves a short ID prefix to a full message ID.
// Returns the full ID if exactly one match is found.
func (d *DB) FindMessageByPrefix(prefix string) (string, error) {
	var ids []string
	rows, err := d.ro().Query("SELECT id FROM messages WHERE id LIKE ?", prefix+"%")
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no message found with prefix %q", prefix)
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("ambiguous prefix %q (%d matches)", prefix, len(ids))
	}
	return ids[0], nil
}

func (d *DB) queryMessages(query string, args ...any) ([]models.Message, error) {
	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []models.Message
	for rows.Next() {
		var m models.Message
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.ReplyTo, &m.Type, &m.Subject, &m.Content, &m.Metadata, &m.CreatedAt, &m.ReadAt, &m.ConversationID, &m.Project, &m.TaskID, &m.Priority, &m.TTLSeconds, &m.ExpiredAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// ExpireMessages marks messages whose TTL has elapsed as expired.
// ttl_seconds=0 means never expires.
func (d *DB) ExpireMessages() (int, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	result, err := d.conn.Exec(
		`UPDATE messages SET expired_at = ?
		 WHERE expired_at IS NULL
		   AND ttl_seconds > 0
		   AND datetime(created_at, '+' || ttl_seconds || ' seconds') < datetime(?)`,
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("expire messages: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
