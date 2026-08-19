package db

import (
	"agent-relay/internal/models"
	"agent-relay/internal/normalize"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// normalizeTTL applies the priority-based TTL policy (cto T6 decision):
//   - P0 never auto-expires (ttl=0) REGARDLESS of the caller's ttl — a critical
//     interrupt stays surfaced until acknowledged, never silently dropped.
//   - P1 defaults to a long horizon (7d) when the caller leaves ttl unspecified (<0).
//   - P2/P3 keep the standard 4h default when unspecified.
//
// A caller-supplied ttl (>=0) is respected for every priority EXCEPT P0, whose
// never-expire semantics are non-negotiable. priority must already be defaulted.
func normalizeTTL(priority string, ttlSeconds int) int {
	if priority == "P0" {
		return 0 // never expires — caller ttl ignored on purpose
	}
	if ttlSeconds < 0 {
		if priority == "P1" {
			return 604800 // 7 days
		}
		return 14400 // 4h
	}
	return ttlSeconds
}

// deriveActionRequired computes the effective comms-discipline action tag
// (DEC-relay-comms-discipline-1) when the caller omits it. Wake-eligible:
// task->do, question/user_question->ask. No-wake reports:
// notification/status/fyi/ack/message/other->none. A response inherits: it
// starts a thread (reply_to nil) => none, else it takes the parent message's
// stored tag (an answer to a wake is itself wake-worthy); a legacy/untagged
// parent => 'do' so an answer to an older thread still wakes (backward-safe).
// The parent read uses the RO pool and never fails the insert.
func (d *DB) deriveActionRequired(msgType string, replyTo *string, project string) string {
	switch msgType {
	case "task":
		return "do"
	case "question", "user_question":
		return "ask"
	case "response":
		if replyTo == nil || *replyTo == "" {
			return "none"
		}
		var parent sql.NullString
		_ = d.ro().QueryRow(
			"SELECT action_required FROM messages WHERE id = ? AND project = ?",
			*replyTo, project,
		).Scan(&parent)
		if parent.Valid && parent.String != "" {
			return parent.String
		}
		return "do"
	case "notification", "status", "fyi", "ack", "message":
		// The named report/no-action types → no-wake.
		return "none"
	default:
		// An UNKNOWN/unlisted type is ambiguous — default it to WAKE, never
		// silently suppress (the SSOT-must-not-go-silent thesis). Only the named
		// report types above route no-wake; a new/odd type errs toward waking.
		return "do"
	}
}

// effectiveActionRequired returns the caller-declared tag if set, else the
// derived one. A declared tag is validated by the handler; this layer trusts it.
func (d *DB) effectiveActionRequired(declared, msgType string, replyTo *string, project string) string {
	if declared != "" {
		return declared
	}
	return d.deriveActionRequired(msgType, replyTo, project)
}

func (d *DB) InsertMessage(project, from, to, msgType, subject, content, metadata, priority string, ttlSeconds int, replyTo, conversationID *string) (*models.Message, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	if priority == "" {
		priority = "P2"
	}
	ttlSeconds = normalizeTTL(priority, ttlSeconds)
	actionRequired := d.deriveActionRequired(msgType, replyTo, project)

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
		ActionRequired: &actionRequired,
	}

	_, err := d.conn.Exec(
		"INSERT INTO messages (id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, conversation_id, project, priority, ttl_seconds, action_required) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		msg.ID, msg.From, msg.To, msg.ReplyTo, msg.Type, msg.Subject, msg.Content, msg.Metadata, msg.CreatedAt, msg.ConversationID, msg.Project, msg.Priority, msg.TTLSeconds, actionRequired,
	)
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	return msg, nil
}

// InsertMessageWithDeliveries inserts a message and its delivery rows in a
// single transaction. The inbox reads the deliveries table, so a message
// inserted without its deliveries (a crash or error between the two writes) is
// silently never delivered. recipients is the already-resolved fan-out list
// (one entry for a direct send, every active peer for a broadcast). Notifying
// recipients and team-inbox bookkeeping stay outside — they are best-effort.
// actionRequired is the caller-declared comms-discipline tag (ask|do|decide|
// none); "" means derive it server-side from (type, reply_to). See
// deriveActionRequired / DEC-relay-comms-discipline-1.
func (d *DB) InsertMessageWithDeliveries(project, from, to, msgType, subject, content, metadata, priority string, ttlSeconds int, replyTo, conversationID *string, recipients []string, actionRequired string) (*models.Message, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	if priority == "" {
		priority = "P2"
	}
	ttlSeconds = normalizeTTL(priority, ttlSeconds)
	effTag := d.effectiveActionRequired(actionRequired, msgType, replyTo, project)

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
		ActionRequired: &effTag,
	}

	tx, err := d.conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	if _, err := tx.Exec(
		"INSERT INTO messages (id, from_agent, to_agent, reply_to, type, subject, content, metadata, created_at, conversation_id, project, priority, ttl_seconds, action_required) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		msg.ID, msg.From, msg.To, msg.ReplyTo, msg.Type, msg.Subject, msg.Content, msg.Metadata, msg.CreatedAt, msg.ConversationID, msg.Project, msg.Priority, msg.TTLSeconds, effTag,
	); err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}

	for i, agent := range recipients {
		if _, err := tx.Exec(
			"INSERT INTO deliveries (id, message_id, to_agent, state, sequence_number, created_at, project) VALUES (?, ?, ?, 'queued', ?, ?, ?)",
			uuid.New().String(), msg.ID, agent, i, now, project,
		); err != nil {
			return nil, fmt.Errorf("create delivery for %s: %w", agent, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit message+deliveries: %w", err)
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

// UnreadCountForAgent returns how many NEW (unseen) messages an agent has —
// deliveries still 'queued', i.e. never surfaced by get_inbox. It is the wake
// signal behind the HTTP unread-count endpoint (issue #17): a poller checks it
// to decide whether to wake the agent. It is non-mutating (unlike GetInbox,
// which marks queued deliveries surfaced) and deliberately excludes 'surfaced'
// deliveries so an already-seen message never re-wakes the agent (WRAITH-2).
func (d *DB) UnreadCountForAgent(project, agentName string) (int, error) {
	if d.HasDeliveries() {
		var n int
		// Count only 'queued' — deliveries the agent has NOT yet been shown. Once
		// a delivery is 'surfaced' (returned by get_inbox at least once) the agent
		// has seen it, so it must not keep re-triggering a wake-up on every poll;
		// it stays visible in the inbox's unread view until an explicit ack. This
		// is the wake signal, deliberately narrower than the get_inbox unread view
		// (queued+surfaced), and is what stops a pile of long-surfaced broadcasts
		// from waking the whole fleet forever (WRAITH-2).
		// Comms-discipline wake predicate (DEC-relay-comms-discipline-1),
		// guard-FIRST: a delivery counts toward the wake signal iff it is P0, OR an
		// ask/dispatch type (question/user_question/task always wake), OR its
		// effective action tag is not 'none'. The guard clauses sit BEFORE the tag
		// check, so a 'none' tag can NEVER suppress a P0/question/task wake even if
		// mis-tagged. COALESCE(action_required,'do') means a legacy/older-client row
		// with NULL tag still WAKES — additive, no fleet break. A deliberate
		// blocker-escalation is a question or an explicitly-tagged send (Ruling-3:
		// passively listing a blocker is not escalating), so it is already covered
		// by these clauses — no fragile 'blocked type' special-case needed.
		err := d.ro().QueryRow(`
			SELECT COUNT(*)
			FROM deliveries d
			JOIN messages m ON d.message_id = m.id
			WHERE d.project = ? AND d.to_agent = ?
			  AND d.state = 'queued'
			  AND m.expired_at IS NULL
			  AND (m.ttl_seconds = 0 OR m.priority = 'P0' OR datetime(m.created_at, '+' || m.ttl_seconds || ' seconds') > datetime('now'))
			  AND (
			        m.priority = 'P0'
			     OR m.type IN ('question','user_question','task')
			     OR COALESCE(m.action_required, 'do') != 'none'
			  )
		`, project, agentName).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("unread count for agent: %w", err)
		}
		return n, nil
	}
	// Legacy DBs (no deliveries table): reuse the read query, which is
	// non-mutating, and count. Legacy inboxes are small and rare.
	msgs, err := d.getInboxLegacy(project, agentName, true, 100000, InboxFilter{})
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

// getInboxLegacy is the inbox query for DBs without deliveries. The recipient
// set is a UNION ALL of three mutually-exclusive, index-driven branches — direct
// DM, broadcast, and conversation — instead of one OR-of-everything predicate.
// The old single OR defeated every index and SCANNED all project messages per
// call (relay-perf R1); each branch here SEARCHes idx_messages_project_to
// (project, to_agent) / idx_messages_conversation, so row selection is
// O(agent-inbox), not O(project). Branches are disjoint (to_agent is one value;
// conversation_id NULL vs NOT NULL), so UNION ALL is correct and skips a dedup
// sort. Common filters (expiry/ttl/unread/priority/from/since) + the final
// priority,created_at ordering wrap the union, preserving the original semantics.
func (d *DB) getInboxLegacy(project, agentName string, unreadOnly bool, limit int, f InboxFilter) ([]models.Message, error) {
	if limit <= 0 {
		limit = 50
	}

	const cols = "m.id, m.from_agent, m.to_agent, m.reply_to, m.type, m.subject, m.content, m.metadata, m.created_at, m.read_at, m.conversation_id, m.project, m.task_id, m.priority, m.ttl_seconds, m.expired_at"

	// Branch A — direct DM to this agent.
	branches := []string{`SELECT ` + cols + ` FROM messages m WHERE m.project = ? AND m.conversation_id IS NULL AND m.to_agent = ?`}
	args := []any{project, agentName}
	// Branch B — broadcast (unless excluded).
	if !f.ExcludeBroadcasts {
		branches = append(branches, `SELECT `+cols+` FROM messages m WHERE m.project = ? AND m.conversation_id IS NULL AND m.to_agent = '*' AND m.from_agent != ?`)
		args = append(args, project, agentName)
	}
	// Branch C — messages in a conversation this agent is an active member of.
	branches = append(branches, `SELECT `+cols+` FROM messages m WHERE m.project = ? AND m.conversation_id IS NOT NULL AND m.conversation_id IN (
			SELECT conversation_id FROM conversation_members WHERE agent_name = ? AND left_at IS NULL
		) AND m.from_agent != ?`)
	args = append(args, project, agentName, agentName)

	query := "SELECT m.id, m.from_agent, m.to_agent, m.reply_to, m.type, m.subject, m.content, m.metadata, m.created_at, m.read_at, m.conversation_id, m.project, m.task_id, m.priority, m.ttl_seconds, m.expired_at FROM (\n" +
		strings.Join(branches, "\nUNION ALL\n") +
		"\n) m WHERE m.expired_at IS NULL AND (m.ttl_seconds = 0 OR m.priority = 'P0' OR datetime(m.created_at, '+' || m.ttl_seconds || ' seconds') > datetime('now'))"

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
// ttl_seconds=0 means never expires. P0 messages NEVER auto-expire regardless
// of ttl (cto T6 policy) — a critical interrupt stays surfaced until acknowledged.
// This priority != 'P0' guard is the sweep-side belt to normalizeTTL's insert-side
// suspenders, so even a legacy/direct-insert P0 with ttl>0 survives every sweep.
func (d *DB) ExpireMessages() (int, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	result, err := d.conn.Exec(
		`UPDATE messages SET expired_at = ?
		 WHERE expired_at IS NULL
		   AND priority != 'P0'
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

// PurgeExpiredMessages hard-deletes messages that were soft-expired (TTL elapsed
// via ExpireMessages) more than `grace` ago, together with their deliveries and
// read receipts. Soft-expiry only hides a message from inboxes; without this the
// messages/deliveries/message_reads tables grow unbounded over long-running
// fleet operation. Messages with ttl_seconds=0 (never expire) are never purged.
// Runs in one transaction on the single writer; returns the message count.
func (d *DB) PurgeExpiredMessages(grace time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-grace).Format("2006-01-02T15:04:05.000000Z")
	const sel = `SELECT id FROM messages WHERE expired_at IS NOT NULL AND datetime(expired_at) < datetime(?)`

	tx, err := d.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("purge messages begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Children first (sqlite FK cascade is off by default).
	if _, err := tx.Exec(`DELETE FROM deliveries WHERE message_id IN (`+sel+`)`, cutoff); err != nil {
		return 0, fmt.Errorf("purge deliveries: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM message_reads WHERE message_id IN (`+sel+`)`, cutoff); err != nil {
		return 0, fmt.Errorf("purge message_reads: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM messages WHERE expired_at IS NOT NULL AND datetime(expired_at) < datetime(?)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("purge messages commit: %w", err)
	}
	return res.RowsAffected()
}
