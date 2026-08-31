package db

import (
	"agent-relay/internal/models"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (d *DB) CreateConversation(project, title, createdBy string, memberNames []string) (*models.Conversation, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")

	conv := &models.Conversation{
		ID:        uuid.New().String(),
		Title:     title,
		CreatedBy: createdBy,
		CreatedAt: now,
		Project:   project,
	}

	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		"INSERT INTO conversations (id, title, created_by, created_at, project) VALUES (?, ?, ?, ?, ?)",
		conv.ID, conv.Title, conv.CreatedBy, conv.CreatedAt, conv.Project,
	); err != nil {
		return nil, fmt.Errorf("insert conversation: %w", err)
	}

	for _, name := range memberNames {
		if _, err := tx.Exec(
			"INSERT INTO conversation_members (conversation_id, agent_name, joined_at) VALUES (?, ?, ?)",
			conv.ID, name, now,
		); err != nil {
			return nil, fmt.Errorf("insert member %s: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return conv, nil
}

func (d *DB) ListConversations(project, agentName string) ([]models.ConversationSummary, error) {
	query := `
		SELECT c.id, c.title, c.created_by, c.created_at,
			(SELECT COUNT(*) FROM conversation_members WHERE conversation_id = c.id AND left_at IS NULL) AS member_count,
			(SELECT COUNT(*) FROM messages m
				WHERE m.conversation_id = c.id
				AND m.from_agent != ?
				AND m.created_at > COALESCE(
					(SELECT last_read_at FROM conversation_reads WHERE conversation_id = c.id AND agent_name = ?),
					'1970-01-01T00:00:00Z'
				)
			) AS unread_count
		FROM conversations c
		JOIN conversation_members cm ON cm.conversation_id = c.id
		WHERE cm.agent_name = ? AND cm.left_at IS NULL AND c.archived_at IS NULL AND c.project = ?
		ORDER BY c.created_at DESC
		LIMIT 30
	`

	rows, err := d.ro().Query(query, agentName, agentName, agentName, project)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var convs []models.ConversationSummary
	for rows.Next() {
		var cs models.ConversationSummary
		if err := rows.Scan(&cs.ID, &cs.Title, &cs.CreatedBy, &cs.CreatedAt, &cs.MemberCount, &cs.UnreadCount); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		cs.Project = project
		convs = append(convs, cs)
	}
	return convs, rows.Err()
}

func (d *DB) GetConversationMessages(conversationID string, limit int) ([]models.Message, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at
		FROM messages
		WHERE conversation_id = ?
		ORDER BY created_at ASC
		LIMIT ?
	`
	return d.queryMessages(query, conversationID, limit)
}

func (d *DB) GetConversationMembers(conversationID string) ([]models.ConversationMember, error) {
	rows, err := d.ro().Query(
		"SELECT conversation_id, agent_name, joined_at, left_at FROM conversation_members WHERE conversation_id = ? AND left_at IS NULL",
		conversationID,
	)
	if err != nil {
		return nil, fmt.Errorf("get members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []models.ConversationMember
	for rows.Next() {
		var m models.ConversationMember
		if err := rows.Scan(&m.ConversationID, &m.AgentName, &m.JoinedAt, &m.LeftAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func (d *DB) IsConversationMember(conversationID, agentName string) (bool, error) {
	var count int
	err := d.ro().QueryRow(
		"SELECT COUNT(*) FROM conversation_members WHERE conversation_id = ? AND agent_name = ? AND left_at IS NULL",
		conversationID, agentName,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (d *DB) AddConversationMember(conversationID, agentName string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")

	// Try to rejoin (clear left_at) or insert new.
	result, err := d.writerExec(
		"UPDATE conversation_members SET left_at = NULL, joined_at = ? WHERE conversation_id = ? AND agent_name = ?",
		now, conversationID, agentName,
	)
	if err != nil {
		return fmt.Errorf("update member: %w", err)
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		return nil
	}

	_, err = d.writerExec(
		"INSERT INTO conversation_members (conversation_id, agent_name, joined_at) VALUES (?, ?, ?)",
		conversationID, agentName, now,
	)
	if err != nil {
		return fmt.Errorf("insert member: %w", err)
	}
	return nil
}

func (d *DB) ConversationExists(conversationID string) (bool, error) {
	var count int
	err := d.ro().QueryRow("SELECT COUNT(*) FROM conversations WHERE id = ?", conversationID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (d *DB) LeaveConversation(conversationID, agentName string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	_, err := d.writerExec(
		"UPDATE conversation_members SET left_at = ? WHERE conversation_id = ? AND agent_name = ? AND left_at IS NULL",
		now, conversationID, agentName,
	)
	return err
}

func (d *DB) ArchiveConversation(conversationID string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	_, err := d.writerExec(
		"UPDATE conversations SET archived_at = ? WHERE id = ? AND archived_at IS NULL",
		now, conversationID,
	)
	return err
}

func (d *DB) MarkConversationRead(conversationID, agentName string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	_, err := d.writerExec(
		`INSERT INTO conversation_reads (conversation_id, agent_name, last_read_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT (conversation_id, agent_name) DO UPDATE SET last_read_at = ?`,
		conversationID, agentName, now, now,
	)
	return err
}
