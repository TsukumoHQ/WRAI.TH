package db

import (
	"time"

	"github.com/google/uuid"
)

// CustomEvent is a user-defined event type with expected meta fields.
type CustomEvent struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MetaFields  string `json:"meta_fields"` // JSON array of field names: ["branch","status","author"]
	Icon        string `json:"icon"`
	CreatedAt   string `json:"created_at"`
}

// UpsertCustomEvent creates or updates a custom event type.
func (d *DB) UpsertCustomEvent(project, name, description, metaFields, icon string) (*CustomEvent, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	id := uuid.New().String()
	if metaFields == "" {
		metaFields = "[]"
	}

	_, err := d.conn.Exec(`
		INSERT INTO custom_events (id, project, name, description, meta_fields, icon, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project, name) DO UPDATE SET
			description = excluded.description,
			meta_fields = excluded.meta_fields,
			icon = excluded.icon`,
		id, project, name, description, metaFields, icon, now)
	if err != nil {
		return nil, err
	}

	// Return the actual row (may have existing ID if updated)
	return d.GetCustomEvent(project, name)
}

// GetCustomEvent returns a single custom event by project + name.
func (d *DB) GetCustomEvent(project, name string) (*CustomEvent, error) {
	row := d.ro().QueryRow(`SELECT id, project, name, description, COALESCE(meta_fields, '[]'), COALESCE(icon, ''), created_at
		FROM custom_events WHERE project = ? AND name = ?`, project, name)

	var e CustomEvent
	err := row.Scan(&e.ID, &e.Project, &e.Name, &e.Description, &e.MetaFields, &e.Icon, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListCustomEvents returns all custom events for a project.
func (d *DB) ListCustomEvents(project string) ([]CustomEvent, error) {
	rows, err := d.ro().Query(`SELECT id, project, name, description, COALESCE(meta_fields, '[]'), COALESCE(icon, ''), created_at
		FROM custom_events WHERE project = ? ORDER BY name LIMIT 200`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CustomEvent
	for rows.Next() {
		var e CustomEvent
		if err := rows.Scan(&e.ID, &e.Project, &e.Name, &e.Description, &e.MetaFields, &e.Icon, &e.CreatedAt); err != nil {
			continue
		}
		result = append(result, e)
	}
	return result, nil
}

// DeleteCustomEvent removes a custom event by ID.
func (d *DB) DeleteCustomEvent(id string) {
	_, _ = d.conn.Exec("DELETE FROM custom_events WHERE id = ?", id)
}
