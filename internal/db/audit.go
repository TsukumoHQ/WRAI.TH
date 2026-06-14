package db

import (
	"agent-relay/internal/models"
	"time"

	"github.com/google/uuid"
)

// RecordAudit appends one entry to the audit trail. Best-effort: a failure here
// must never block the action it describes, so callers ignore the error.
func (d *DB) RecordAudit(e models.AuditEntry) error {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.CreatedAt == "" {
		e.CreatedAt = time.Now().UTC().Format(memoryTimeFmt)
	}
	if e.ResourceType == "" {
		e.ResourceType = "task"
	}
	if e.Project == "" {
		e.Project = "default"
	}
	_, err := d.conn.Exec(
		`INSERT INTO audit_log (id, project, actor, action, resource_type, resource_id, summary, details, reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Project, e.Actor, e.Action, e.ResourceType, e.ResourceID,
		e.Summary, e.Details, e.Reason, e.CreatedAt,
	)
	return err
}

// ListAudit returns the most recent audit entries for a project, optionally
// scoped to a single resource (e.g. one task). Newest first.
func (d *DB) ListAudit(project, resourceID string, limit int) ([]models.AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id, project, actor, action, resource_type, resource_id, summary, details, reason, created_at
		FROM audit_log WHERE project = ?`
	args := []any{project}
	if resourceID != "" {
		query += " AND resource_id = ?"
		args = append(args, resourceID)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.ro().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []models.AuditEntry{}
	for rows.Next() {
		var e models.AuditEntry
		if err := rows.Scan(&e.ID, &e.Project, &e.Actor, &e.Action, &e.ResourceType,
			&e.ResourceID, &e.Summary, &e.Details, &e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
