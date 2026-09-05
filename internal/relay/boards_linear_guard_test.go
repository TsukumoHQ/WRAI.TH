package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// seedBoardWithLinearTask creates a board in project and a single source='linear'
// mirror task placed on it (via UpdateTaskFields, the board_id setter). status is
// the mirror task's native status. Returns the board id + slug and the task id.
func seedBoardWithLinearTask(t *testing.T, h *Handlers, project, status string) (boardID, slug, taskID string) {
	t.Helper()
	b, err := h.db.CreateBoard(project, "Board", "board", "", "creator")
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	taskID = "lt-" + status
	if err := h.db.UpsertLinearTask(db.LinearTaskSeed{ID: taskID, Project: project, Title: "mirror", Status: status}); err != nil {
		t.Fatalf("upsert linear task: %v", err)
	}
	if _, err := h.db.UpdateTaskFields(taskID, project, "test", nil, nil, nil, &b.ID, nil, nil, nil, nil); err != nil {
		t.Fatalf("place linear task on board: %v", err)
	}
	return b.ID, b.Slug, taskID
}

// expectBoardLinearRefusal asserts a typed BOARD_HAS_LINEAR_TASKS envelope and
// returns the parsed body for further field assertions.
func expectBoardLinearRefusal(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatal("expected a typed BOARD_HAS_LINEAR_TASKS error, got success/nil")
	}
	raw := res.Content[0].(mcp.TextContent).Text
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("refusal is not structured JSON (client can't branch on it): %v\nraw: %s", err, raw)
	}
	if body["code"] != "BOARD_HAS_LINEAR_TASKS" {
		t.Fatalf("want code BOARD_HAS_LINEAR_TASKS, got %v\nraw: %s", body["code"], raw)
	}
	return body
}

// TestArchiveBoardRefusesOpenLinearTasks (AC1): a board with an OPEN mirrored
// task refuses archive with the typed code + a count, and writes NO archived_at
// on either the board or the task.
func TestArchiveBoardRefusesOpenLinearTasks(t *testing.T) {
	h := testHandlers(t)
	boardID, slug, taskID := seedBoardWithLinearTask(t, h, "p1", "in-progress")

	res, _ := h.HandleArchiveBoard(ctx, call(map[string]any{"project": "p1", "board_id": boardID}))
	body := expectBoardLinearRefusal(t, res)
	if msg, _ := body["message"].(string); !strings.Contains(msg, "1 open Linear-mirrored") {
		t.Fatalf("message must carry the count + remedy, got: %v", body["message"])
	}

	board, err := h.db.GetBoard("p1", slug)
	if err != nil || board == nil {
		t.Fatalf("get board: %v (nil=%v)", err, board == nil)
	}
	if board.ArchivedAt != nil {
		t.Fatalf("board must NOT be archived after refusal, got archived_at=%v", *board.ArchivedAt)
	}
	task, err := h.db.GetTask(taskID, "p1")
	if err != nil || task == nil {
		t.Fatalf("get task: %v (nil=%v)", err, task == nil)
	}
	if task.ArchivedAt != nil {
		t.Fatalf("mirror task must NOT be archived after refusal, got archived_at=%v", *task.ArchivedAt)
	}
}

// TestArchiveBoardAllowsTerminalLinearTasks (AC2): a board whose only mirrored
// task is terminal (done) archives freely — the cascade closing out a done
// mirror row is not a desync.
func TestArchiveBoardAllowsTerminalLinearTasks(t *testing.T) {
	h := testHandlers(t)
	boardID, slug, taskID := seedBoardWithLinearTask(t, h, "p1", "done")

	res, _ := h.HandleArchiveBoard(ctx, call(map[string]any{"project": "p1", "board_id": boardID}))
	if res.IsError {
		t.Fatalf("archive must succeed when mirrored tasks are terminal: %s", expectError(t, res))
	}
	board, _ := h.db.GetBoard("p1", slug)
	if board == nil || board.ArchivedAt == nil {
		t.Fatal("board should be archived")
	}
	task, _ := h.db.GetTask(taskID, "p1")
	if task == nil || task.ArchivedAt == nil {
		t.Fatal("terminal mirror task should be archived by the cascade, as today")
	}
}

// TestDeleteBoardRefusesAnyLinearTask (AC3): delete_board is refused while ANY
// mirrored task references the board (here a terminal, already-archived one, so
// archive itself succeeds), and the board row is left untouched.
func TestDeleteBoardRefusesAnyLinearTask(t *testing.T) {
	h := testHandlers(t)
	boardID, slug, _ := seedBoardWithLinearTask(t, h, "p1", "done")

	// Archive first (allowed: task is terminal) — cascade archives the board and
	// the done mirror task, so the board is delete-eligible by the archived rule.
	if err := h.db.ArchiveBoard("p1", boardID); err != nil {
		t.Fatalf("archive board: %v", err)
	}

	res, _ := h.HandleDeleteBoard(ctx, call(map[string]any{"project": "p1", "board_id": boardID}))
	body := expectBoardLinearRefusal(t, res)
	if msg, _ := body["message"].(string); !strings.Contains(msg, "1 Linear-mirrored tasks still reference") {
		t.Fatalf("delete refusal must carry the count, got: %v", body["message"])
	}
	if board, _ := h.db.GetBoard("p1", slug); board == nil {
		t.Fatal("board row must be untouched (still present) after a delete refusal")
	}
}

// TestNativeBoardArchiveDeleteUnchanged (AC4): a board with only native tasks
// archives (cascading its tasks) and then deletes exactly as before.
func TestNativeBoardArchiveDeleteUnchanged(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "worker", nil)
	b, err := h.db.CreateBoard("p1", "Native", "native", "", "creator")
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	dispRes, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "worker", "profile": "dev", "title": "native task", "board_id": b.ID,
	}))
	taskID := parseJSON(t, dispRes)["task"].(map[string]any)["id"].(string)

	if res, _ := h.HandleArchiveBoard(ctx, call(map[string]any{"project": "p1", "board_id": b.ID})); res.IsError {
		t.Fatalf("native board archive must behave as today: %s", expectError(t, res))
	}
	if board, _ := h.db.GetBoard("p1", "native"); board == nil || board.ArchivedAt == nil {
		t.Fatal("native board should be archived")
	}
	if task, _ := h.db.GetTask(taskID, "p1"); task == nil || task.ArchivedAt == nil {
		t.Fatal("native task should be archived by the cascade, as today")
	}
	if res, _ := h.HandleDeleteBoard(ctx, call(map[string]any{"project": "p1", "board_id": b.ID})); res.IsError {
		t.Fatalf("native board delete must behave as today: %s", expectError(t, res))
	}
	if board, _ := h.db.GetBoard("p1", "native"); board != nil {
		t.Fatal("native board should be gone after delete")
	}
}

// AC5 (fail-closed on a db error) lives in the db package as
// TestBoardGuardFailsClosedOnDBError (internal/db/boards_failclosed_test.go): it
// drops the tasks table so the guard's COUNT errors while the boards DELETE
// would still succeed — a non-tautological injected error a closed handle can't
// express from here.

// TestBoardLinearGuardErrorEnvelope (AC6): the refusal is permission-category
// and non-retryable so a client parks instead of hot-looping.
func TestBoardLinearGuardErrorEnvelope(t *testing.T) {
	h := testHandlers(t)
	boardID, _, _ := seedBoardWithLinearTask(t, h, "p1", "in-progress")

	res, _ := h.HandleArchiveBoard(ctx, call(map[string]any{"project": "p1", "board_id": boardID}))
	body := expectBoardLinearRefusal(t, res)
	if body["errorCategory"] != CategoryPermission {
		t.Fatalf("BOARD_HAS_LINEAR_TASKS must be permission-category, got %v", body["errorCategory"])
	}
	if body["isRetryable"] != false {
		t.Fatalf("BOARD_HAS_LINEAR_TASKS must be non-retryable, got %v", body["isRetryable"])
	}
}

// TestBoardLinearGuardMessageContract (AC1/AC3): the archive and delete refusals
// carry their exact, distinct remedy strings with the count.
func TestBoardLinearGuardMessageContract(t *testing.T) {
	archiveErr := (&db.LinearTasksOnBoardError{Op: "archive", Count: 3}).Error()
	if archiveErr != "3 open Linear-mirrored tasks on this board — move_task them off or close them in Linear first" {
		t.Fatalf("archive remedy string drifted: %q", archiveErr)
	}
	deleteErr := (&db.LinearTasksOnBoardError{Op: "delete", Count: 2}).Error()
	if deleteErr != "2 Linear-mirrored tasks still reference this board" {
		t.Fatalf("delete remedy string drifted: %q", deleteErr)
	}
}
