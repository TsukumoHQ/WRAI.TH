package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-relay/internal/models"
	"agent-relay/internal/normalize"

	"github.com/google/uuid"
)

const memoryTimeFmt = "2006-01-02T15:04:05.000000Z"

// SetMemory creates or versions a memory. If upsert is true, overwrites
// existing values silently (archives old version). If false, flags a conflict.
func (d *DB) SetMemory(project, agentName, key, value, tagsJSON, scope, confidence, layer string, upsert ...bool) (*models.Memory, error) {
	doUpsert := true
	if len(upsert) > 0 {
		doUpsert = upsert[0]
	}
	value = normalize.JSONKeys(value)
	now := time.Now().UTC().Format(memoryTimeFmt)
	if confidence == "" {
		confidence = "stated"
	}
	if tagsJSON == "" {
		tagsJSON = "[]"
	}
	if layer == "" {
		layer = "behavior"
	}

	// Wrap the read-modify-write in a BEGIN IMMEDIATE transaction so SQLite
	// acquires the write lock before the SELECT. Without this, concurrent
	// writers on the same key can both read the same max version and both
	// insert as version+1, breaking the supersedes chain.
	tx, err := d.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := d.findActiveMemoryTx(tx, project, scope, agentName, key)
	if err != nil {
		return nil, err
	}

	id := uuid.New().String()

	if existing != nil {
		if existing.Value == value {
			// Same value — just update timestamp
			_, err := tx.Exec(
				`UPDATE memories SET updated_at = ?, tags = ?, confidence = ? WHERE id = ?`,
				now, tagsJSON, confidence, existing.ID,
			)
			if err != nil {
				return nil, fmt.Errorf("update memory: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("commit memory noop: %w", err)
			}
			existing.UpdatedAt = now
			existing.Tags = tagsJSON
			existing.Confidence = confidence
			return existing, nil
		}

		if doUpsert {
			// Upsert mode — archive old version and insert new one silently
			_, archErr := tx.Exec(
				`UPDATE memories SET archived_at = ?, archived_by = ? WHERE id = ?`,
				now, "upsert", existing.ID,
			)
			if archErr != nil {
				return nil, fmt.Errorf("archive old memory: %w", archErr)
			}
			mem := &models.Memory{
				ID:         id,
				Key:        key,
				Value:      value,
				Tags:       tagsJSON,
				Scope:      scope,
				Project:    project,
				AgentName:  agentName,
				Confidence: confidence,
				Version:    existing.Version + 1,
				Supersedes: &existing.ID,
				CreatedAt:  now,
				UpdatedAt:  now,
				Layer:      layer,
			}
			_, err := tx.Exec(
				`INSERT INTO memories (id, key, value, tags, scope, project, agent_name, confidence, version, supersedes, created_at, updated_at, layer)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				mem.ID, mem.Key, mem.Value, mem.Tags, mem.Scope, mem.Project,
				mem.AgentName, mem.Confidence, mem.Version, mem.Supersedes,
				mem.CreatedAt, mem.UpdatedAt, mem.Layer,
			)
			if err != nil {
				return nil, fmt.Errorf("insert upserted memory: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("commit upsert: %w", err)
			}
			return mem, nil
		}

		// Conflict mode — create new version, flag conflict
		mem := &models.Memory{
			ID:           id,
			Key:          key,
			Value:        value,
			Tags:         tagsJSON,
			Scope:        scope,
			Project:      project,
			AgentName:    agentName,
			Confidence:   confidence,
			Version:      existing.Version + 1,
			Supersedes:   &existing.ID,
			ConflictWith: &existing.ID,
			CreatedAt:    now,
			UpdatedAt:    now,
			Layer:        layer,
		}

		_, err := tx.Exec(
			`INSERT INTO memories (id, key, value, tags, scope, project, agent_name, confidence, version, supersedes, conflict_with, created_at, updated_at, layer)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			mem.ID, mem.Key, mem.Value, mem.Tags, mem.Scope, mem.Project,
			mem.AgentName, mem.Confidence, mem.Version, mem.Supersedes, mem.ConflictWith,
			mem.CreatedAt, mem.UpdatedAt, mem.Layer,
		)
		if err != nil {
			return nil, fmt.Errorf("insert conflicting memory: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit conflict: %w", err)
		}
		return mem, nil
	}

	// No existing memory — create fresh
	mem := &models.Memory{
		ID:         id,
		Key:        key,
		Value:      value,
		Tags:       tagsJSON,
		Scope:      scope,
		Project:    project,
		AgentName:  agentName,
		Confidence: confidence,
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
		Layer:      layer,
	}

	_, err = tx.Exec(
		`INSERT INTO memories (id, key, value, tags, scope, project, agent_name, confidence, version, created_at, updated_at, layer)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mem.ID, mem.Key, mem.Value, mem.Tags, mem.Scope, mem.Project,
		mem.AgentName, mem.Confidence, mem.Version, mem.CreatedAt, mem.UpdatedAt, mem.Layer,
	)
	if err != nil {
		return nil, fmt.Errorf("insert memory: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit insert: %w", err)
	}
	return mem, nil
}

// findActiveMemoryTx finds the newest active memory version through an open
// write transaction. Required so SetMemory holds the lock from read to
// write and prevents the version race observed under concurrent writers
// on the same key.
func (d *DB) findActiveMemoryTx(tx *sql.Tx, project, scope, agentName, key string) (*models.Memory, error) {
	var query string
	var args []any
	switch scope {
	case "agent":
		query = `SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
			 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
			 FROM memories WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL
			 ORDER BY version DESC LIMIT 1`
		args = []any{key, project, agentName}
	case "project":
		query = `SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
			 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
			 FROM memories WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL
			 ORDER BY version DESC LIMIT 1`
		args = []any{key, project}
	case "global":
		query = `SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
			 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
			 FROM memories WHERE key = ? AND scope = 'global' AND archived_at IS NULL
			 ORDER BY version DESC LIMIT 1`
		args = []any{key}
	default:
		return nil, fmt.Errorf("invalid scope: %s", scope)
	}
	row := tx.QueryRow(query, args...)
	var m models.Memory
	err := row.Scan(&m.ID, &m.Key, &m.Value, &m.Tags, &m.Scope, &m.Project, &m.AgentName,
		&m.Confidence, &m.Version, &m.Supersedes, &m.ConflictWith, &m.CreatedAt,
		&m.UpdatedAt, &m.ArchivedAt, &m.ArchivedBy, &m.Layer)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetMemory retrieves a memory by key with scope cascade: agent → project → global.
func (d *DB) GetMemory(project, agentName, key, scope string) ([]models.Memory, error) {
	// If a specific scope is requested, search that scope only (+ check for conflicts)
	if scope != "" {
		return d.getMemoryAtScope(project, agentName, key, scope)
	}

	// Cascade: agent → project → global
	for _, s := range []string{"agent", "project", "global"} {
		results, err := d.getMemoryAtScope(project, agentName, key, s)
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			return results, nil
		}
	}
	return []models.Memory{}, nil
}

func (d *DB) getMemoryAtScope(project, agentName, key, scope string) ([]models.Memory, error) {
	var query string
	var args []any

	switch scope {
	case "agent":
		query = `SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
				 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
				 FROM memories WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL
				 ORDER BY version DESC`
		args = []any{key, project, agentName}
	case "project":
		query = `SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
				 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
				 FROM memories WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL
				 ORDER BY version DESC`
		args = []any{key, project}
	case "global":
		query = `SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
				 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
				 FROM memories WHERE key = ? AND scope = 'global' AND archived_at IS NULL
				 ORDER BY version DESC`
		args = []any{key}
	default:
		return nil, fmt.Errorf("invalid scope: %s", scope)
	}

	return d.queryMemories(query, args...)
}

// SearchMemory performs full-text search across memories.
func (d *DB) SearchMemory(project, agentName, query string, tags []string, scope string, limit int) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 20
	}

	// Build WHERE clauses for the main table filter
	var conditions []string
	var args []any

	conditions = append(conditions, "m.archived_at IS NULL")

	if scope != "" {
		switch scope {
		case "agent":
			conditions = append(conditions, "m.scope = 'agent'", "m.project = ?", "m.agent_name = ?")
			args = append(args, project, agentName)
		case "project":
			conditions = append(conditions, "m.scope IN ('project', 'agent')", "m.project = ?")
			args = append(args, project)
		case "global":
			conditions = append(conditions, "m.scope = 'global'")
		}
	} else {
		// Cross-scope search: show project + global + own agent memories
		conditions = append(conditions, "(m.scope = 'global' OR (m.project = ? AND (m.scope = 'project' OR (m.scope = 'agent' AND m.agent_name = ?))))")
		args = append(args, project, agentName)
	}

	if len(tags) > 0 {
		for _, tag := range tags {
			conditions = append(conditions, "m.tags LIKE ?")
			args = append(args, "%\""+tag+"\"%")
		}
	}

	where := strings.Join(conditions, " AND ")

	var sql string
	if query != "" {
		sql = fmt.Sprintf(
			`SELECT m.id, m.key, m.value, m.tags, m.scope, m.project, m.agent_name, m.confidence, m.version,
			 m.supersedes, m.conflict_with, m.created_at, m.updated_at, m.archived_at, m.archived_by, m.layer
			 FROM memories m
			 JOIN memories_fts f ON m.rowid = f.rowid
			 WHERE %s AND memories_fts MATCH ?
			 ORDER BY rank
			 LIMIT ?`, where,
		)
		args = append(args, escapeFTSQuery(query), limit)
	} else {
		sql = fmt.Sprintf(
			`SELECT m.id, m.key, m.value, m.tags, m.scope, m.project, m.agent_name, m.confidence, m.version,
			 m.supersedes, m.conflict_with, m.created_at, m.updated_at, m.archived_at, m.archived_by, m.layer
			 FROM memories m
			 WHERE %s
			 ORDER BY m.updated_at DESC
			 LIMIT ?`, where,
		)
		args = append(args, limit)
	}

	return d.queryMemories(sql, args...)
}

// ListMemories returns memories matching the given filters.
func (d *DB) ListMemories(project, scope, agentName string, tags []string, limit int) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 50
	}

	var conditions []string
	var args []any

	conditions = append(conditions, "archived_at IS NULL")

	if project != "" {
		conditions = append(conditions, "project = ?")
		args = append(args, project)
	}
	if scope != "" {
		conditions = append(conditions, "scope = ?")
		args = append(args, scope)
	}
	if agentName != "" {
		conditions = append(conditions, "agent_name = ?")
		args = append(args, agentName)
	}
	if len(tags) > 0 {
		for _, tag := range tags {
			conditions = append(conditions, "tags LIKE ?")
			args = append(args, "%\""+tag+"\"%")
		}
	}

	where := strings.Join(conditions, " AND ")
	args = append(args, limit)

	q := fmt.Sprintf(
		`SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
		 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
		 FROM memories WHERE %s ORDER BY updated_at DESC LIMIT ?`, where,
	)

	return d.queryMemories(q, args...)
}

// DeleteMemory soft-deletes a memory (sets archived_at).
func (d *DB) DeleteMemory(project, agentName, key, scope string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)

	var query string
	var args []any

	switch scope {
	case "agent":
		query = `UPDATE memories SET archived_at = ?, archived_by = ? WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL`
		args = []any{now, agentName, key, project, agentName}
	case "project":
		query = `UPDATE memories SET archived_at = ?, archived_by = ? WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL`
		args = []any{now, agentName, key, project}
	case "global":
		query = `UPDATE memories SET archived_at = ?, archived_by = ? WHERE key = ? AND scope = 'global' AND archived_at IS NULL`
		args = []any{now, agentName, key}
	default:
		return fmt.Errorf("invalid scope: %s", scope)
	}

	res, err := d.conn.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory not found: %s (scope=%s)", key, scope)
	}
	return nil
}

// ResolveConflict resolves a conflict by setting the chosen value and archiving alternatives.
func (d *DB) ResolveConflict(project, agentName, key, chosenValue, scope string) (*models.Memory, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)

	// Find all active memories at this key+scope
	var memories []models.Memory
	var err error

	switch scope {
	case "agent":
		memories, err = d.queryMemories(
			`SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
			 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
			 FROM memories WHERE key = ? AND scope = 'agent' AND project = ? AND agent_name = ? AND archived_at IS NULL
			 ORDER BY version DESC`,
			key, project, agentName,
		)
	case "project":
		memories, err = d.queryMemories(
			`SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
			 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
			 FROM memories WHERE key = ? AND scope = 'project' AND project = ? AND archived_at IS NULL
			 ORDER BY version DESC`,
			key, project,
		)
	case "global":
		memories, err = d.queryMemories(
			`SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
			 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
			 FROM memories WHERE key = ? AND scope = 'global' AND archived_at IS NULL
			 ORDER BY version DESC`,
			key,
		)
	default:
		return nil, fmt.Errorf("invalid scope: %s", scope)
	}
	if err != nil {
		return nil, err
	}

	if len(memories) == 0 {
		return nil, fmt.Errorf("no active memories found for key=%s scope=%s", key, scope)
	}

	// Find the winner (exact match on value) or create a new resolution
	var winner *models.Memory
	var losers []models.Memory

	for i := range memories {
		if memories[i].Value == chosenValue {
			winner = &memories[i]
		} else {
			losers = append(losers, memories[i])
		}
	}

	tx, err := d.conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Archive all losers
	for _, l := range losers {
		_, err := tx.Exec(
			`UPDATE memories SET archived_at = ?, archived_by = ? WHERE id = ?`,
			now, "conflict_resolution", l.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("archive loser: %w", err)
		}
	}

	if winner != nil {
		// Clear conflict flag on winner
		_, err := tx.Exec(
			`UPDATE memories SET conflict_with = NULL, updated_at = ? WHERE id = ?`,
			now, winner.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("clear conflict: %w", err)
		}
		winner.ConflictWith = nil
		winner.UpdatedAt = now
	} else {
		// Neither matched — create a new resolution memory, archive all
		for _, m := range memories {
			if m.ArchivedAt == nil { // not already archived above
				_, err := tx.Exec(
					`UPDATE memories SET archived_at = ?, archived_by = ? WHERE id = ?`,
					now, "conflict_resolution", m.ID,
				)
				if err != nil {
					return nil, fmt.Errorf("archive for resolution: %w", err)
				}
			}
		}

		// Collect tags from the highest-version memory
		highestTags := memories[0].Tags
		highestVersion := memories[0].Version

		id := uuid.New().String()
		highestLayer := memories[0].Layer
		if highestLayer == "" {
			highestLayer = "behavior"
		}
		winner = &models.Memory{
			ID:         id,
			Key:        key,
			Value:      chosenValue,
			Tags:       highestTags,
			Scope:      scope,
			Project:    project,
			AgentName:  agentName,
			Confidence: "stated",
			Version:    highestVersion + 1,
			Supersedes: &memories[0].ID,
			CreatedAt:  now,
			UpdatedAt:  now,
			Layer:      highestLayer,
		}

		_, err := tx.Exec(
			`INSERT INTO memories (id, key, value, tags, scope, project, agent_name, confidence, version, supersedes, created_at, updated_at, layer)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			winner.ID, winner.Key, winner.Value, winner.Tags, winner.Scope, winner.Project,
			winner.AgentName, winner.Confidence, winner.Version, winner.Supersedes,
			winner.CreatedAt, winner.UpdatedAt, winner.Layer,
		)
		if err != nil {
			return nil, fmt.Errorf("insert resolution: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return winner, nil
}

// GetMemoriesByLayer returns all active memories for an agent filtered by layer.
// Cross-scope: returns agent + project + global memories (same cascade as search).
func (d *DB) GetMemoriesByLayer(project, agentName, layer string) ([]models.Memory, error) {
	return d.queryMemories(
		`SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
		 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
		 FROM memories
		 WHERE archived_at IS NULL AND layer = ?
		   AND (scope = 'global' OR (project = ? AND (scope = 'project' OR (scope = 'agent' AND agent_name = ?))))
		 ORDER BY updated_at DESC`,
		layer, project, agentName,
	)
}

// ListAllMemories returns all active memories across projects (for web UI).
func (d *DB) ListAllMemories(limit int) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 200
	}
	return d.queryMemories(
		`SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
		 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
		 FROM memories WHERE archived_at IS NULL ORDER BY updated_at DESC LIMIT ?`,
		limit,
	)
}

// SearchAllMemories searches across all projects (for web UI).
func (d *DB) SearchAllMemories(query string, limit int) ([]models.Memory, error) {
	if limit <= 0 {
		limit = 50
	}
	if query == "" {
		return d.ListAllMemories(limit)
	}
	return d.queryMemories(
		`SELECT m.id, m.key, m.value, m.tags, m.scope, m.project, m.agent_name, m.confidence, m.version,
		 m.supersedes, m.conflict_with, m.created_at, m.updated_at, m.archived_at, m.archived_by, m.layer
		 FROM memories m
		 JOIN memories_fts f ON m.rowid = f.rowid
		 WHERE m.archived_at IS NULL AND memories_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		escapeFTSQuery(query), limit,
	)
}

// escapeFTSQuery wraps each token in double quotes so FTS5 does not interpret
// punctuation (especially hyphens) as column filters or operators. Empty tokens
// are skipped. A bare token stays a bare token, but `state-machine` becomes
// `"state-machine"` and is treated as a literal string to search.
func escapeFTSQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	// Fields() splits on any whitespace
	tokens := strings.Fields(q)
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		// Escape embedded double-quotes by doubling them (FTS5 convention)
		t = strings.ReplaceAll(t, `"`, `""`)
		out = append(out, `"`+t+`"`)
	}
	return strings.Join(out, " ")
}

// DeleteMemoryByID soft-deletes a specific memory by ID (for web UI).
func (d *DB) DeleteMemoryByID(id, archivedBy string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	res, err := d.conn.Exec(
		`UPDATE memories SET archived_at = ?, archived_by = ? WHERE id = ? AND archived_at IS NULL`,
		now, archivedBy, id,
	)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory not found: %s", id)
	}
	return nil
}

// GetMemoryByID returns a single memory by ID (for web UI).
func (d *DB) GetMemoryByID(id string) (*models.Memory, error) {
	mems, err := d.queryMemories(
		`SELECT id, key, value, tags, scope, project, agent_name, confidence, version,
		 supersedes, conflict_with, created_at, updated_at, archived_at, archived_by, layer
		 FROM memories WHERE id = ?`, id,
	)
	if err != nil {
		return nil, err
	}
	if len(mems) == 0 {
		return nil, nil
	}
	return &mems[0], nil
}

// MemoryStats returns summary stats for the CLI/API.
func (d *DB) MemoryStats(project string) (total int, conflicts int, err error) {
	q := `SELECT COUNT(*), COALESCE(SUM(CASE WHEN conflict_with IS NOT NULL THEN 1 ELSE 0 END), 0) FROM memories WHERE archived_at IS NULL`
	var args []any
	if project != "" {
		q += " AND project = ?"
		args = append(args, project)
	}
	err = d.ro().QueryRow(q, args...).Scan(&total, &conflicts)
	return
}

// --- internal helpers ---

func (d *DB) queryMemories(query string, args ...any) ([]models.Memory, error) {
	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []models.Memory
	for rows.Next() {
		var m models.Memory
		err := rows.Scan(
			&m.ID, &m.Key, &m.Value, &m.Tags, &m.Scope, &m.Project,
			&m.AgentName, &m.Confidence, &m.Version,
			&m.Supersedes, &m.ConflictWith,
			&m.CreatedAt, &m.UpdatedAt, &m.ArchivedAt, &m.ArchivedBy, &m.Layer,
		)
		if err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// ParseTags parses a JSON array of strings, returning the tags slice.
func ParseTags(tagsJSON string) []string {
	if tagsJSON == "" || tagsJSON == "[]" {
		return nil
	}
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	return tags
}

// TagsToJSON converts a string slice to a JSON array string.
func TagsToJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(tags)
	return string(b)
}
