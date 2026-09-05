package db

import (
	"database/sql"
	"testing"
	"time"
)

// danglingTask dispatches a native task with an explicit board_id (a non-nil
// board_id skips DispatchTask's board-resolution guard, so a bogus/ghost id is
// stored verbatim — exactly the dangling-pointer shape we test).
func danglingTask(t *testing.T, d *DB, project, profile, boardID string) string {
	t.Helper()
	var bp *string
	if boardID != "" {
		bp = &boardID
	}
	task, err := d.DispatchTask(project, profile, "cto", "task", "", "P2", nil, bp, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch %s: %v", profile, err)
	}
	return task.ID
}

// boardOf reads a task's current board_id.
func boardOf(t *testing.T, d *DB, taskID string) string {
	t.Helper()
	var b sql.NullString
	if err := d.conn.QueryRow("SELECT board_id FROM tasks WHERE id = ?", taskID).Scan(&b); err != nil {
		t.Fatalf("read board_id %s: %v", taskID, err)
	}
	return b.String
}

// archiveBoardRaw stamps a board archived_at directly, WITHOUT the ArchiveBoard
// cascade (which would also archive the board's tasks and thus disqualify them
// as candidates). This reproduces the archived-board dangling case in isolation.
func archiveBoardRaw(t *testing.T, d *DB, boardID string) {
	t.Helper()
	now := time.Now().UTC().Format(memoryTimeFmt)
	if _, err := d.conn.Exec("UPDATE boards SET archived_at = ? WHERE id = ?", now, boardID); err != nil {
		t.Fatalf("archive board raw %s: %v", boardID, err)
	}
}

func rawExec(t *testing.T, d *DB, query string, args ...interface{}) {
	t.Helper()
	if _, err := d.conn.Exec(query, args...); err != nil {
		t.Fatalf("raw exec: %v", err)
	}
}

// AC1: dry-run (default) reports both a missing-board and an archived-board task
// as action=rehome with the ProductBoardSlugForProfile target, and changes zero
// rows.
func TestSweepDanglingBoardsDryRun(t *testing.T) {
	d := testDB(t)
	target, err := d.CreateBoard("p", "Wraith", "wraith", "", "cto")
	if err != nil {
		t.Fatalf("create target board: %v", err)
	}

	missing := danglingTask(t, d, "p", "wraith-backend", "ghost-board")

	stale, err := d.CreateBoard("p", "Old", "old", "", "cto")
	if err != nil {
		t.Fatalf("create stale board: %v", err)
	}
	archivedBoardTask := danglingTask(t, d, "p", "wraith-backend", stale.ID)
	archiveBoardRaw(t, d, stale.ID)

	res, err := d.SweepDanglingBoards(false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !res.DryRun {
		t.Fatalf("expected DryRun true")
	}
	if len(res.Dispositions) != 2 {
		t.Fatalf("dispositions = %d, want 2: %+v", len(res.Dispositions), res.Dispositions)
	}
	for _, disp := range res.Dispositions {
		if disp.Action != "rehome" {
			t.Fatalf("action = %q, want rehome: %+v", disp.Action, disp)
		}
		if disp.Slug != "wraith" || disp.ToBoard != target.ID {
			t.Fatalf("target = %q/%q, want wraith/%s: %+v", disp.Slug, disp.ToBoard, target.ID, disp)
		}
	}
	// Zero rows changed.
	if got := boardOf(t, d, missing); got != "ghost-board" {
		t.Fatalf("missing task board_id = %q, want unchanged ghost-board", got)
	}
	if got := boardOf(t, d, archivedBoardTask); got != stale.ID {
		t.Fatalf("archived-board task board_id = %q, want unchanged %s", got, stale.ID)
	}
}

// AC2: apply re-homes both tasks onto the active product board and is idempotent
// (a second pass finds zero candidates).
func TestSweepDanglingBoardsApplyIdempotent(t *testing.T) {
	d := testDB(t)
	target, err := d.CreateBoard("p", "Wraith", "wraith", "", "cto")
	if err != nil {
		t.Fatalf("create target board: %v", err)
	}
	missing := danglingTask(t, d, "p", "wraith-backend", "ghost-board")
	stale, err := d.CreateBoard("p", "Old", "old", "", "cto")
	if err != nil {
		t.Fatalf("create stale board: %v", err)
	}
	archivedBoardTask := danglingTask(t, d, "p", "wraith-backend", stale.ID)
	archiveBoardRaw(t, d, stale.ID)

	res, err := d.SweepDanglingBoards(true)
	if err != nil {
		t.Fatalf("apply sweep: %v", err)
	}
	if res.DryRun {
		t.Fatalf("expected DryRun false in apply mode")
	}
	if len(res.Dispositions) != 2 {
		t.Fatalf("dispositions = %d, want 2", len(res.Dispositions))
	}
	if got := boardOf(t, d, missing); got != target.ID {
		t.Fatalf("missing task board_id = %q, want %s", got, target.ID)
	}
	if got := boardOf(t, d, archivedBoardTask); got != target.ID {
		t.Fatalf("archived-board task board_id = %q, want %s", got, target.ID)
	}

	// Idempotence: nothing left to re-home.
	res2, err := d.SweepDanglingBoards(true)
	if err != nil {
		t.Fatalf("second apply sweep: %v", err)
	}
	if res2.Scanned != 0 || len(res2.Dispositions) != 0 {
		t.Fatalf("second pass not idempotent: scanned=%d dispositions=%+v", res2.Scanned, res2.Dispositions)
	}
}

// AC3: the sweep never deletes and never blanks a board_id — a candidate whose
// project has no active target board is reported action=no-target and left
// exactly as is (row, status, archived_at, board_id all intact).
func TestSweepDanglingBoardsNeverDeleteNoTarget(t *testing.T) {
	d := testDB(t)
	// analytics-lead → slug "backlog"; no "backlog" board exists in "p".
	task := danglingTask(t, d, "p", "analytics-lead", "ghost-board")

	res, err := d.SweepDanglingBoards(true)
	if err != nil {
		t.Fatalf("apply sweep: %v", err)
	}
	if len(res.Dispositions) != 1 {
		t.Fatalf("dispositions = %d, want 1: %+v", len(res.Dispositions), res.Dispositions)
	}
	disp := res.Dispositions[0]
	if disp.Action != "no-target" || disp.Slug != "backlog" || disp.ToBoard != "" {
		t.Fatalf("disposition = %+v, want no-target/backlog/empty", disp)
	}

	// Never delete / never mutate: row present, board_id, status, archived_at intact.
	var status, boardID string
	var archivedAt sql.NullString
	if err := d.conn.QueryRow(
		"SELECT status, board_id, archived_at FROM tasks WHERE id = ?", task,
	).Scan(&status, &boardID, &archivedAt); err != nil {
		t.Fatalf("task row missing after sweep: %v", err)
	}
	if boardID != "ghost-board" {
		t.Fatalf("board_id = %q, want unchanged ghost-board", boardID)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want unchanged pending", status)
	}
	if archivedAt.Valid {
		t.Fatalf("archived_at = %q, want still NULL", archivedAt.String)
	}
}

// AC4: scope — a source='linear' task and an archived task, both with a dangling
// board_id, are NOT candidates (an active target board exists, so they would be
// re-homed if in scope; they must not be).
func TestSweepDanglingBoardsScope(t *testing.T) {
	d := testDB(t)
	if _, err := d.CreateBoard("p", "Wraith", "wraith", "", "cto"); err != nil {
		t.Fatalf("create target board: %v", err)
	}

	linear := danglingTask(t, d, "p", "wraith-backend", "ghost-board")
	rawExec(t, d, "UPDATE tasks SET source = 'linear' WHERE id = ?", linear)

	archived := danglingTask(t, d, "p", "wraith-backend", "ghost-board")
	now := time.Now().UTC().Format(memoryTimeFmt)
	rawExec(t, d, "UPDATE tasks SET archived_at = ? WHERE id = ?", now, archived)

	res, err := d.SweepDanglingBoards(true)
	if err != nil {
		t.Fatalf("apply sweep: %v", err)
	}
	if res.Scanned != 0 || len(res.Dispositions) != 0 {
		t.Fatalf("expected zero candidates, got scanned=%d dispositions=%+v", res.Scanned, res.Dispositions)
	}
	if got := boardOf(t, d, linear); got != "ghost-board" {
		t.Fatalf("linear task board_id = %q, want unchanged (reconcile owns it)", got)
	}
	if got := boardOf(t, d, archived); got != "ghost-board" {
		t.Fatalf("archived task board_id = %q, want unchanged (out of scope)", got)
	}
}
