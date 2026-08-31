package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
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
	// Correlation (trace_id v1) — auto-derived from the task when the caller
	// left it unset, never a required field. Best-effort: a lookup miss just
	// leaves it empty, mirroring messages' deriveTraceID idiom.
	if e.TraceID == "" && e.ResourceType == "task" && e.ResourceID != "" {
		var t sql.NullString
		_ = d.ro().QueryRow("SELECT trace_id FROM tasks WHERE id = ? AND project = ?", e.ResourceID, e.Project).Scan(&t)
		if t.Valid {
			e.TraceID = t.String
		}
	}
	_, err := d.writerExec(
		`INSERT INTO audit_log (id, project, actor, action, resource_type, resource_id, summary, details, reason, created_at, trace_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Project, e.Actor, e.Action, e.ResourceType, e.ResourceID,
		e.Summary, e.Details, e.Reason, e.CreatedAt, e.TraceID,
	)
	return err
}

// ListAudit returns the most recent audit entries for a project, optionally
// scoped to a single resource (e.g. one task). Newest first.
func (d *DB) ListAudit(project, resourceID string, limit int) ([]models.AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id, project, actor, action, resource_type, resource_id, summary, details, reason, created_at, trace_id
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
		var traceID sql.NullString
		if err := rows.Scan(&e.ID, &e.Project, &e.Actor, &e.Action, &e.ResourceType,
			&e.ResourceID, &e.Summary, &e.Details, &e.Reason, &e.CreatedAt, &traceID); err != nil {
			return nil, err
		}
		e.TraceID = traceID.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeOldAuditLog hard-deletes audit entries older than maxAge so the audit_log
// table doesn't grow unbounded. The audit trail is the accountability record, so
// it's retained far longer than messages (see AuditLogRetention). Returns the
// number of rows deleted.
func (d *DB) PurgeOldAuditLog(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(memoryTimeFmt)
	res, err := d.writerExec(`DELETE FROM audit_log WHERE datetime(created_at) < datetime(?)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge audit log: %w", err)
	}
	return res.RowsAffected()
}
