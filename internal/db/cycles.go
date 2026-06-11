package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// UpsertCycle creates or updates a cycle definition.
func (d *DB) UpsertCycle(project, name, prompt string, ttl int) (*models.Cycle, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	if ttl <= 0 {
		ttl = 10
	}

	var existingID string
	err := d.conn.QueryRow("SELECT id FROM cycles WHERE project = ? AND name = ?", project, name).Scan(&existingID)
	if err == sql.ErrNoRows {
		id := uuid.New().String()
		_, err := d.conn.Exec(
			"INSERT INTO cycles (id, project, name, prompt, ttl, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			id, project, name, prompt, ttl, now, now,
		)
		if err != nil {
			return nil, fmt.Errorf("insert cycle: %w", err)
		}
		return &models.Cycle{ID: id, Project: project, Name: name, Prompt: prompt, TTL: ttl, CreatedAt: now, UpdatedAt: now}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check cycle: %w", err)
	}

	_, err = d.conn.Exec(
		"UPDATE cycles SET prompt = ?, ttl = ?, updated_at = ? WHERE id = ?",
		prompt, ttl, now, existingID,
	)
	if err != nil {
		return nil, fmt.Errorf("update cycle: %w", err)
	}
	return &models.Cycle{ID: existingID, Project: project, Name: name, Prompt: prompt, TTL: ttl, UpdatedAt: now}, nil
}

// GetCycle returns a cycle by project + name.
func (d *DB) GetCycle(project, name string) (*models.Cycle, error) {
	var c models.Cycle
	err := d.ro().QueryRow(
		"SELECT id, project, name, prompt, ttl, created_at, updated_at FROM cycles WHERE project = ? AND name = ?",
		project, name,
	).Scan(&c.ID, &c.Project, &c.Name, &c.Prompt, &c.TTL, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get cycle: %w", err)
	}
	return &c, nil
}

// ListCycles returns all cycles for a project.
func (d *DB) ListCycles(project string) ([]models.Cycle, error) {
	rows, err := d.ro().Query(
		"SELECT id, project, name, prompt, ttl, created_at, updated_at FROM cycles WHERE project = ? ORDER BY name LIMIT 200",
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("list cycles: %w", err)
	}
	defer rows.Close()

	var result []models.Cycle
	for rows.Next() {
		var c models.Cycle
		if err := rows.Scan(&c.ID, &c.Project, &c.Name, &c.Prompt, &c.TTL, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// DeleteCycle removes a cycle by project + name.
func (d *DB) DeleteCycle(project, name string) error {
	_, err := d.conn.Exec("DELETE FROM cycles WHERE project = ? AND name = ?", project, name)
	return err
}
