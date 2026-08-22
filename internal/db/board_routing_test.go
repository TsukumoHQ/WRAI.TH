package db

import "testing"

// ProductBoardSlugForProfile is the single mapping funneled by both the
// dispatch-time auto-router and the backfill — pin every rule named in the
// ticket (goal c254c55d) plus the fallback.
func TestProductBoardSlugForProfile(t *testing.T) {
	cases := []struct {
		profile string
		want    string
	}{
		{"yoru-frontend", "yoru"},
		{"yoru-backend", "yoru"},
		{"yoru", "yoru"},
		{"wraith-engine", "wraith"},
		{"wraith-engine-2", "wraith"},
		{"wraith-backend", "wraith"},
		{"wraith", "wraith"},
		{"trovex-backend", "trovex"},
		{"trovex-frontend", "trovex"},
		{"trovex", "trovex"},
		{"backend-lead", "niwa"},
		{"backend-lead-2", "niwa"},
		{"gate-lead", "niwa"},
		{"frontend-lead", "niwa"},
		{"cli-lead", "niwa"},
		{"niwa-cto", "niwa"},
		{"niwa-anything", "niwa"},
		{"dev", "backlog"},
		{"human", "backlog"},
		{"", "backlog"},
	}
	for _, tc := range cases {
		if got := ProductBoardSlugForProfile(tc.profile); got != tc.want {
			t.Errorf("ProductBoardSlugForProfile(%q) = %q, want %q", tc.profile, got, tc.want)
		}
	}
}

// DispatchTask, on a project with more than one board, must auto-route by
// profile instead of refusing — a niwa lead lands on the Niwa board, a
// yoru-frontend dispatch lands on Yoru, and an explicit board_id still wins.
func TestDispatchTaskAutoRoutesToProductBoard(t *testing.T) {
	d := testDB(t)
	d.EnsureProject("p1")
	_, _ = d.CreateBoard("p1", "Trovex", "trovex", "", "cto")
	wraith, _ := d.CreateBoard("p1", "Wraith", "wraith", "", "cto")
	yoru, _ := d.CreateBoard("p1", "Yoru", "yoru", "", "cto")
	niwa, _ := d.CreateBoard("p1", "Niwa", "niwa", "", "cto")

	task, err := d.DispatchTask("p1", "yoru-frontend", "cto", "yoru work", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("yoru-frontend dispatch must auto-route, not refuse: %v", err)
	}
	if task.BoardID == nil || *task.BoardID != yoru.ID {
		t.Errorf("expected yoru board %s, got %v", yoru.ID, task.BoardID)
	}

	task2, err := d.DispatchTask("p1", "backend-lead", "cto", "niwa work", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("niwa lead dispatch must auto-route, not refuse: %v", err)
	}
	if task2.BoardID == nil || *task2.BoardID != niwa.ID {
		t.Errorf("expected niwa board %s, got %v", niwa.ID, task2.BoardID)
	}

	explicit := wraith.ID
	task3, err := d.DispatchTask("p1", "yoru-frontend", "cto", "explicit wins", "", "P2", nil, &explicit, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("explicit board_id must still dispatch: %v", err)
	}
	if task3.BoardID == nil || *task3.BoardID != wraith.ID {
		t.Errorf("explicit board_id must win over auto-routing, got %v want %s", task3.BoardID, wraith.ID)
	}
}

// An unmapped profile routes to the "backlog" board when one exists, rather
// than refusing — only a genuinely unmapped board (no backlog board in the
// project either) still hits the ambiguous-refusal.
func TestDispatchTaskUnknownProfileRoutesToBacklogBoard(t *testing.T) {
	d := testDB(t)
	d.EnsureProject("p1")
	_, _ = d.CreateBoard("p1", "Trovex", "trovex", "", "cto")
	_, _ = d.CreateBoard("p1", "Wraith", "wraith", "", "cto")
	backlog, _ := d.CreateBoard("p1", "Backlog", "backlog", "", "cto")

	task, err := d.DispatchTask("p1", "dev", "cto", "unmapped profile", "", "P2", nil, nil, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("unmapped profile must fall back to the backlog board, not refuse: %v", err)
	}
	if task.BoardID == nil || *task.BoardID != backlog.ID {
		t.Errorf("expected backlog board %s, got %v", backlog.ID, task.BoardID)
	}
}

// backfillProductBoardRouting moves existing active tasks (dispatched before
// the auto-router existed, or created via a path that bypassed it) onto their
// mapped product board, and leaves done/cancelled tasks and already-correct
// tasks alone.
func TestBackfillProductBoardRouting(t *testing.T) {
	d := testDB(t)
	d.EnsureProject("p1")
	trovex, _ := d.CreateBoard("p1", "Trovex", "trovex", "", "cto")
	wraith, _ := d.CreateBoard("p1", "Wraith", "wraith", "", "cto")

	// Mis-boarded: a wraith-engine task landed on Trovex (single-board-era default).
	misboarded, err := d.DispatchTask("p1", "wraith-engine", "cto", "misboarded", "", "P2", nil, &trovex.ID, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Already correct: leave it alone (idempotency probe — must not error, must not "move" again).
	correct, err := d.DispatchTask("p1", "wraith-engine", "cto", "already correct", "", "P2", nil, &wraith.ID, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Done task, mis-boarded but terminal: must NOT move.
	doneTask, err := d.DispatchTask("p1", "wraith-engine", "cto", "done but misboarded", "", "P2", nil, &trovex.ID, TypedTicket{}, false, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	result := "shipped"
	if _, err := d.CompleteTask(doneTask.ID, "wraith-engine", "p1", &result); err != nil {
		t.Fatalf("complete: %v", err)
	}

	counts, err := backfillProductBoardRouting(d.conn)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if counts["wraith"] != 1 {
		t.Fatalf("expected 1 task moved to wraith, got counts=%v", counts)
	}

	got, _ := d.GetTask(misboarded.ID, "p1")
	if got.BoardID == nil || *got.BoardID != wraith.ID {
		t.Errorf("misboarded task should now be on wraith board, got %v", got.BoardID)
	}
	stillCorrect, _ := d.GetTask(correct.ID, "p1")
	if stillCorrect.BoardID == nil || *stillCorrect.BoardID != wraith.ID {
		t.Errorf("already-correct task should be untouched, got %v", stillCorrect.BoardID)
	}
	stillDone, _ := d.GetTask(doneTask.ID, "p1")
	if stillDone.BoardID == nil || *stillDone.BoardID != trovex.ID {
		t.Errorf("done task must NOT be moved by the backfill, got %v", stillDone.BoardID)
	}

	// Re-run: idempotent, moves nothing more.
	counts2, err := backfillProductBoardRouting(d.conn)
	if err != nil {
		t.Fatalf("backfill re-run: %v", err)
	}
	if len(counts2) != 0 {
		t.Errorf("re-run must be a no-op, got counts=%v", counts2)
	}
}
