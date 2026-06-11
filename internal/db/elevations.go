package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// GrantElevation creates a temporary privilege escalation for an agent.
func (d *DB) GrantElevation(project, agent, role, grantedBy, reason string, duration time.Duration) (*models.Elevation, error) {
	now := time.Now().UTC()
	id := uuid.New().String()
	expiresAt := now.Add(duration).Format(memoryTimeFmt)

	_, err := d.conn.Exec(`INSERT INTO elevated_privileges (id, project, agent_name, elevated_role, granted_by, reason, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, project, agent, role, grantedBy, reason, expiresAt, now.Format(memoryTimeFmt))
	if err != nil {
		return nil, fmt.Errorf("grant elevation: %w", err)
	}

	return &models.Elevation{
		ID: id, Project: project, AgentName: agent, ElevatedRole: role,
		GrantedBy: grantedBy, Reason: reason, ExpiresAt: expiresAt,
		CreatedAt: now.Format(memoryTimeFmt),
	}, nil
}

// RevokeElevation immediately revokes a privilege escalation.
func (d *DB) RevokeElevation(id string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	_, err := d.conn.Exec(`UPDATE elevated_privileges SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, id)
	return err
}

// GetActiveElevation returns the active (non-expired, non-revoked) elevation for an agent.
func (d *DB) GetActiveElevation(project, agent string) (*models.Elevation, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	var e models.Elevation
	err := d.ro().QueryRow(`SELECT id, project, agent_name, elevated_role, granted_by, reason, expires_at, revoked_at, created_at
		FROM elevated_privileges
		WHERE project = ? AND agent_name = ? AND revoked_at IS NULL AND expires_at > ?
		ORDER BY created_at DESC LIMIT 1`, project, agent, now).Scan(
		&e.ID, &e.Project, &e.AgentName, &e.ElevatedRole, &e.GrantedBy, &e.Reason, &e.ExpiresAt, &e.RevokedAt, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListActiveElevations returns all active elevations for a project.
func (d *DB) ListActiveElevations(project string) ([]models.Elevation, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	rows, err := d.ro().Query(`SELECT id, project, agent_name, elevated_role, granted_by, reason, expires_at, revoked_at, created_at
		FROM elevated_privileges
		WHERE project = ? AND revoked_at IS NULL AND expires_at > ?
		ORDER BY created_at DESC LIMIT 200`, project, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.Elevation
	for rows.Next() {
		var e models.Elevation
		if err := rows.Scan(&e.ID, &e.Project, &e.AgentName, &e.ElevatedRole, &e.GrantedBy, &e.Reason, &e.ExpiresAt, &e.RevokedAt, &e.CreatedAt); err != nil {
			continue
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ExpireElevations marks all expired elevations as revoked.
// Returns the number of elevations expired.
func (d *DB) ExpireElevations() (int64, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	result, err := d.conn.Exec(`UPDATE elevated_privileges SET revoked_at = ? WHERE revoked_at IS NULL AND expires_at <= ?`, now, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
