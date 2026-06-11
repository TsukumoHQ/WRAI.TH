package db

import (
	"agent-relay/internal/models"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const boardColumns = "id, project, name, slug, description, created_by, created_at, archived_at"

func scanBoard(row interface{ Scan(...any) error }) (models.Board, error) {
	var b models.Board
	err := row.Scan(&b.ID, &b.Project, &b.Name, &b.Slug, &b.Description, &b.CreatedBy, &b.CreatedAt, &b.ArchivedAt)
	return b, err
}

func (d *DB) CreateBoard(project, name, slug, description, createdBy string) (*models.Board, error) {
	now := time.Now().UTC().Format(memoryTimeFmt)
	b := &models.Board{
		ID:          uuid.New().String(),
		Project:     project,
		Name:        name,
		Slug:        slug,
		Description: description,
		CreatedBy:   createdBy,
		CreatedAt:   now,
	}

	_, err := d.conn.Exec(
		`INSERT INTO boards (id, project, name, slug, description, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.Project, b.Name, b.Slug, b.Description, b.CreatedBy, b.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create board: %w", err)
	}
	return b, nil
}

func (d *DB) ListBoards(project string) ([]models.Board, error) {
	rows, err := d.ro().Query(
		`SELECT `+boardColumns+` FROM boards WHERE project = ? AND archived_at IS NULL ORDER BY created_at LIMIT 200`,
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("list boards: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var boards []models.Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		boards = append(boards, b)
	}
	return boards, rows.Err()
}

func (d *DB) ListAllBoards() ([]models.Board, error) {
	rows, err := d.ro().Query(
		`SELECT ` + boardColumns + ` FROM boards WHERE archived_at IS NULL ORDER BY project, created_at LIMIT 500`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all boards: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var boards []models.Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		boards = append(boards, b)
	}
	return boards, rows.Err()
}

func (d *DB) GetBoard(project, slug string) (*models.Board, error) {
	b, err := scanBoard(d.ro().QueryRow(
		`SELECT `+boardColumns+` FROM boards WHERE project = ? AND slug = ?`,
		project, slug,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get board: %w", err)
	}
	return &b, nil
}

// ArchiveBoard soft-deletes a board and archives all its tasks.
func (d *DB) ArchiveBoard(project, boardID string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)

	_, err := d.conn.Exec(
		`UPDATE boards SET archived_at = ? WHERE id = ? AND project = ? AND archived_at IS NULL`,
		now, boardID, project,
	)
	if err != nil {
		return fmt.Errorf("archive board: %w", err)
	}

	// Also archive all tasks on this board
	_, err = d.conn.Exec(
		`UPDATE tasks SET archived_at = ? WHERE board_id = ? AND project = ? AND archived_at IS NULL`,
		now, boardID, project,
	)
	if err != nil {
		return fmt.Errorf("archive board tasks: %w", err)
	}
	return nil
}

// DeleteBoard hard-deletes a board (only if already archived).
func (d *DB) DeleteBoard(project, boardID string) error {
	_, err := d.conn.Exec(
		`DELETE FROM boards WHERE id = ? AND project = ? AND archived_at IS NOT NULL`,
		boardID, project,
	)
	if err != nil {
		return fmt.Errorf("delete board: %w", err)
	}
	return nil
}
