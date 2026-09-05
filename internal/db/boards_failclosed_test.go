package db

import "testing"

// TestBoardGuardFailsClosedOnDBError (AC5): the board-level Linear guard is
// fail-closed AND non-tautological. We drop the tasks table so the guard's
// existence COUNT errors while the board DELETE itself (which touches only the
// boards table) would still succeed. With the guard, DeleteBoard REFUSES and the
// archived board row survives; a neutered guard would DELETE the board and
// return nil. That distinguishes fail-closed from fail-open — which a closed db
// handle (where every write fails regardless) cannot.
func TestBoardGuardFailsClosedOnDBError(t *testing.T) {
	d := testDB(t)
	b, err := d.CreateBoard("p1", "B", "b", "", "creator")
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	// DeleteBoard requires archived_at NOT NULL; archive it (no linear tasks yet).
	if err := d.ArchiveBoard("p1", b.ID); err != nil {
		t.Fatalf("archive board: %v", err)
	}

	// Injected error: the guard reads `tasks`; the DELETE writes only `boards`.
	if _, err := d.conn.Exec("DROP TABLE tasks"); err != nil {
		t.Fatalf("inject db error (drop tasks): %v", err)
	}

	if err := d.DeleteBoard("p1", b.ID); err == nil {
		t.Fatal("fail-closed: DeleteBoard must REFUSE when the Linear check cannot run, not delete the board")
	}

	// The board must survive: the guard returned before the DELETE could run.
	got, gerr := d.GetBoard("p1", "b")
	if gerr != nil {
		t.Fatalf("get board after refusal: %v", gerr)
	}
	if got == nil {
		t.Fatal("board was deleted despite the check failing — that is fail-OPEN, the exact bug AC5 guards against")
	}
}
