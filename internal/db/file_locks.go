package db

import (
	"agent-relay/internal/models"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ClaimFiles creates a file lock for the given agent and file paths.
func (d *DB) ClaimFiles(project, agentName, filePaths string, ttlSeconds int) (*models.FileLock, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	if ttlSeconds <= 0 {
		ttlSeconds = 1800
	}

	lock := &models.FileLock{
		ID:         uuid.New().String(),
		AgentName:  agentName,
		Project:    project,
		FilePaths:  filePaths,
		ClaimedAt:  now,
		TTLSeconds: ttlSeconds,
	}

	_, err := d.conn.Exec(
		"INSERT INTO file_locks (id, agent_name, project, file_paths, claimed_at, ttl_seconds) VALUES (?, ?, ?, ?, ?, ?)",
		lock.ID, lock.AgentName, lock.Project, lock.FilePaths, lock.ClaimedAt, lock.TTLSeconds,
	)
	if err != nil {
		return nil, fmt.Errorf("claim files: %w", err)
	}
	return lock, nil
}

// ReleaseFiles marks active locks for the given agent+paths as released.
func (d *DB) ReleaseFiles(project, agentName, filePaths string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	_, err := d.conn.Exec(
		"UPDATE file_locks SET released_at = ? WHERE agent_name = ? AND project = ? AND file_paths = ? AND released_at IS NULL",
		now, agentName, project, filePaths,
	)
	return err
}

// ListFileLocks returns active (non-released, non-expired) file locks for a project.
func (d *DB) ListFileLocks(project string) ([]models.FileLock, error) {
	// Expiry is flagged by the background cleanup ticker (StartCleanup). To avoid a
	// write-lock acquire on every read, we don't expire here — instead the SELECT
	// filters out TTL-elapsed locks by timestamp so the view is correct immediately.
	rows, err := d.ro().Query(
		`SELECT id, agent_name, project, file_paths, claimed_at, released_at, ttl_seconds
		 FROM file_locks
		 WHERE project = ? AND released_at IS NULL
		   AND datetime(claimed_at, '+' || ttl_seconds || ' seconds') > datetime('now')
		 ORDER BY claimed_at DESC LIMIT 500`,
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("list file locks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var locks []models.FileLock
	for rows.Next() {
		var l models.FileLock
		if err := rows.Scan(&l.ID, &l.AgentName, &l.Project, &l.FilePaths, &l.ClaimedAt, &l.ReleasedAt, &l.TTLSeconds); err != nil {
			return nil, fmt.Errorf("scan file lock: %w", err)
		}
		locks = append(locks, l)
	}
	return locks, rows.Err()
}

// ExpireFileLocks marks locks whose TTL has elapsed as released.
func (d *DB) ExpireFileLocks() (int, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	result, err := d.conn.Exec(
		`UPDATE file_locks SET released_at = ?
		 WHERE released_at IS NULL
		   AND datetime(claimed_at, '+' || ttl_seconds || ' seconds') < datetime(?)`,
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("expire file locks: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
