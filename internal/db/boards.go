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

	_, err := d.writerExec(
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

// LinearTasksOnBoardError refuses a board archive/delete that would silently
// desync Linear-mirrored tasks. Boards carry no Linear coupling of their own;
// the hazard is the task side — archive cascades archived_at onto open mirror
// rows, delete orphans their board_id (DEC-wraith-boards-linear-guard-1). Op is
// "archive" or "delete"; Count is the number of offending source='linear' tasks
// (open for archive, any for delete). The relay handler maps it to the typed
// BOARD_HAS_LINEAR_TASKS refusal.
type LinearTasksOnBoardError struct {
	Op    string
	Count int
}

func (e *LinearTasksOnBoardError) Error() string {
	if e.Op == "delete" {
		return fmt.Sprintf("%d Linear-mirrored tasks still reference this board", e.Count)
	}
	return fmt.Sprintf("%d open Linear-mirrored tasks on this board — move_task them off or close them in Linear first", e.Count)
}

// ArchiveBoard soft-deletes a board and archives all its tasks. It refuses,
// fail-closed and inside the same writer tx as the cascade, when the board
// carries any OPEN Linear-mirrored task (source='linear', not archived, not
// done/cancelled): the cascade would stamp archived_at onto a live mirror row
// and silently desync it from Linear. A board whose mirrored tasks are all
// terminal/archived archives freely, exactly as before.
func (d *DB) ArchiveBoard(project, boardID string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)

	tx, err := d.beginWriterTx()
	if err != nil {
		return fmt.Errorf("archive board begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Guard BEFORE the cascade. A query error refuses (fail closed) — the
	// destructive UPDATEs below never run, so the board is left untouched.
	var open int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM tasks
		   WHERE board_id = ? AND project = ? AND source = 'linear'
		     AND archived_at IS NULL AND status NOT IN ('done','cancelled')`,
		boardID, project,
	).Scan(&open); err != nil {
		return fmt.Errorf("archive board linear check: %w", err)
	}
	if open > 0 {
		return &LinearTasksOnBoardError{Op: "archive", Count: open}
	}

	if _, err := tx.Exec(
		`UPDATE boards SET archived_at = ? WHERE id = ? AND project = ? AND archived_at IS NULL`,
		now, boardID, project,
	); err != nil {
		return fmt.Errorf("archive board: %w", err)
	}
	// Also archive all tasks on this board.
	if _, err := tx.Exec(
		`UPDATE tasks SET archived_at = ? WHERE board_id = ? AND project = ? AND archived_at IS NULL`,
		now, boardID, project,
	); err != nil {
		return fmt.Errorf("archive board tasks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("archive board commit: %w", err)
	}
	return nil
}

// DeleteBoard hard-deletes a board (only if already archived). It refuses,
// fail-closed and inside the same writer tx as the delete, when ANY task — any
// status, archived included — still references the board via source='linear':
// deleting the board would orphan those mirror rows' board_id
// (DEC-wraith-boards-linear-guard-1).
func (d *DB) DeleteBoard(project, boardID string) error {
	tx, err := d.beginWriterTx()
	if err != nil {
		return fmt.Errorf("delete board begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Guard BEFORE the delete. A query error refuses (fail closed) — the DELETE
	// below never runs, so the board row is left untouched.
	var refs int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE board_id = ? AND project = ? AND source = 'linear'`,
		boardID, project,
	).Scan(&refs); err != nil {
		return fmt.Errorf("delete board linear check: %w", err)
	}
	if refs > 0 {
		return &LinearTasksOnBoardError{Op: "delete", Count: refs}
	}

	if _, err := tx.Exec(
		`DELETE FROM boards WHERE id = ? AND project = ? AND archived_at IS NOT NULL`,
		boardID, project,
	); err != nil {
		return fmt.Errorf("delete board: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete board commit: %w", err)
	}
	return nil
}
