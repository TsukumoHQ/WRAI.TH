package db

import (
	"agent-relay/internal/models"
	"fmt"
)

// ListAllConversations returns all non-archived conversations with member names for a project.
func (d *DB) ListAllConversations(project string) ([]models.ConversationWithMembers, error) {
	query := `
		SELECT c.id, c.title, c.created_by, c.created_at,
			(SELECT COUNT(*) FROM messages WHERE conversation_id = c.id) AS message_count
		FROM conversations c
		WHERE c.archived_at IS NULL AND c.project = ?
		ORDER BY c.created_at DESC
	`

	rows, err := d.ro().Query(query, project)
	if err != nil {
		return nil, fmt.Errorf("list all conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var convs []models.ConversationWithMembers
	for rows.Next() {
		var c models.ConversationWithMembers
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedBy, &c.CreatedAt, &c.MessageCount); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		c.Project = project
		convs = append(convs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fetch members for each conversation
	for i := range convs {
		members, err := d.GetConversationMembers(convs[i].ID)
		if err != nil {
			return nil, fmt.Errorf("get members for %s: %w", convs[i].ID, err)
		}
		for _, m := range members {
			convs[i].Members = append(convs[i].Members, m.AgentName)
		}
	}

	return convs, nil
}

// GetAllRecentMessages returns the most recent messages across all conversations for a project.
func (d *DB) GetAllRecentMessages(project string, limit int) ([]models.Message, error) {
	if limit <= 0 {
		limit = 200
	}

	query := `
		SELECT id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at
		FROM messages
		WHERE project = ?
		ORDER BY created_at DESC
		LIMIT ?
	`
	return d.queryMessages(query, project, limit)
}

// GetMessagesSince returns all messages created after the given RFC3339 timestamp for a project.
func (d *DB) GetMessagesSince(project, since string, limit int) ([]models.Message, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `
		SELECT id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at
		FROM messages
		WHERE project = ? AND created_at > ?
		ORDER BY created_at ASC
		LIMIT ?
	`
	return d.queryMessages(query, project, since, limit)
}

// ListAllAgents returns all agents across all projects, ordered by project then name.
func (d *DB) ListAllAgents() ([]models.Agent, error) {
	rows, err := d.ro().Query("SELECT " + agentColumns + " FROM agents WHERE status IN ('active', 'sleeping', 'inactive') ORDER BY project, name")
	if err != nil {
		return nil, fmt.Errorf("list all agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var agents []models.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// GetAllRecentMessagesAllProjects returns recent messages across all projects.
func (d *DB) GetAllRecentMessagesAllProjects(limit int) ([]models.Message, error) {
	if limit <= 0 {
		limit = 200
	}

	query := `
		SELECT id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at
		FROM messages
		ORDER BY created_at DESC
		LIMIT ?
	`
	return d.queryMessages(query, limit)
}

// GetMessagesSinceAllProjects returns messages created after the given timestamp across all projects.
func (d *DB) GetMessagesSinceAllProjects(since string, limit int) ([]models.Message, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `
		SELECT id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, read_at, conversation_id, project, task_id, priority, ttl_seconds, expired_at
		FROM messages
		WHERE created_at > ?
		ORDER BY created_at ASC
		LIMIT ?
	`
	return d.queryMessages(query, since, limit)
}

// ListAllConversationsAcrossProjects returns all non-archived conversations across all projects.
func (d *DB) ListAllConversationsAcrossProjects() ([]models.ConversationWithMembers, error) {
	query := `
		SELECT c.id, c.title, c.created_by, c.created_at, c.project,
			(SELECT COUNT(*) FROM messages WHERE conversation_id = c.id) AS message_count
		FROM conversations c
		WHERE c.archived_at IS NULL
		ORDER BY c.created_at DESC
	`

	rows, err := d.ro().Query(query)
	if err != nil {
		return nil, fmt.Errorf("list all conversations across projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var convs []models.ConversationWithMembers
	for rows.Next() {
		var c models.ConversationWithMembers
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedBy, &c.CreatedAt, &c.Project, &c.MessageCount); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		convs = append(convs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fetch members for each conversation
	for i := range convs {
		members, err := d.GetConversationMembers(convs[i].ID)
		if err != nil {
			return nil, fmt.Errorf("get members for %s: %w", convs[i].ID, err)
		}
		for _, m := range members {
			convs[i].Members = append(convs[i].Members, m.AgentName)
		}
	}

	return convs, nil
}

// ListProjects returns all distinct project names across agents, messages, and conversations.
func (d *DB) ListProjects() ([]string, error) {
	rows, err := d.ro().Query(`
		SELECT DISTINCT project FROM (
			SELECT project FROM agents
			UNION
			SELECT project FROM messages
			UNION
			SELECT project FROM conversations
		) ORDER BY project
	`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}
