package relay

import (
	"strings"
	"testing"
)

// TestV2Boards is the FOUNDER "boards dans la console v2" contract (task
// 515a10e7): the board-blind v2 kanban gains a board selector, a client-side
// board_id filter, and a board-name chip on the card — reusing the existing
// GET /api/boards route (zero new route). The five subtests map 1:1 to the five
// acceptance criteria. Static asset assertions mirror console_v2_delete_test.go
// (readAsset + strings.Contains); AC1 also exercises the live REST endpoint the
// UI calls. No JS runtime harness exists in this repo, so the client-side
// behaviour is pinned at the source level (stated where it matters).
func TestV2Boards(t *testing.T) {
	// AC1 — board.js calls api.boards(project) and renders a selector: 'Tous' +
	// boards + '(sans board)'. The wrapper exists in api.js and the live route
	// returns the project's boards.
	t.Run("BoardsCallAndSelector", func(t *testing.T) {
		api := readAsset(t, "static/v2/api.js")
		if !strings.Contains(api, "boards:") || !strings.Contains(api, "/api/boards?") {
			t.Error("api.js does not expose a boards(project) → GET /api/boards wrapper")
		}
		board := readAsset(t, "static/v2/board.js")
		if !strings.Contains(board, "ctx.api.boards(") {
			t.Error("board.js does not call ctx.api.boards() to load boards")
		}
		for _, marker := range []string{"board-pill", "data-board=", "'Tous'", "(sans board)", "renderBoardFilter"} {
			if !strings.Contains(board, marker) {
				t.Errorf("board.js selector missing marker %q", marker)
			}
		}

		// live: the route the UI calls returns the project's active boards.
		r := testRelay(t)
		b, err := r.DB.CreateBoard("p1", "Backend", "backend", "", "tester")
		if err != nil {
			t.Fatalf("create board: %v", err)
		}
		res := doAPI(r, "GET", "/boards?project=p1", "")
		arr := decodeJSONArray(t, res)
		if len(arr) != 1 {
			t.Fatalf("GET /api/boards: want 1 board, got %d", len(arr))
		}
		m, _ := arr[0].(map[string]any)
		if got, _ := m["id"].(string); got != b.ID {
			t.Errorf("GET /api/boards: want board id %q, got %q", b.ID, got)
		}
	})

	// AC2 — selecting a board filters the kanban client-side by board_id; 'Tous'
	// (the default) is a no-op that preserves the current behaviour. Pinned at the
	// source: applyFilters branches only when boardSel !== 'all', and boardSel
	// initialises to 'all'.
	t.Run("ClientSideBoardFilter", func(t *testing.T) {
		board := readAsset(t, "static/v2/board.js")
		if !strings.Contains(board, "boardSel = 'all'") {
			t.Error("board.js must default boardSel to 'all' (Tous = current behaviour unchanged)")
		}
		if !strings.Contains(board, "if (boardSel !== 'all')") {
			t.Error("applyFilters must no-op when boardSel === 'all'")
		}
		if !strings.Contains(board, "t.board_id !== boardSel") {
			t.Error("a selected board must filter tasks by board_id client-side")
		}
		// the filter lives inside applyFilters (the single kanban filter choke).
		af := strings.Index(board, "function applyFilters")
		branch := strings.Index(board, "if (boardSel !== 'all')")
		next := strings.Index(board, "const filtersActive")
		if af < 0 || branch < 0 || next < 0 || af >= branch || branch >= next {
			t.Error("the board_id filter must sit inside applyFilters")
		}
	})

	// AC3 — the card shows a board-name chip when board_id resolves; a dangling or
	// empty board_id yields no chip and never throws (DEC-wraith-referential-
	// integrity tolerance). Pinned: the chip is gated on the resolved name from
	// boardById, which is built from the fetched boards.
	t.Run("BoardChipResolvedDanglingSafe", func(t *testing.T) {
		board := readAsset(t, "static/v2/board.js")
		if !strings.Contains(board, "kcard-board") {
			t.Fatal("board.js has no board-name chip (kcard-board) on the card")
		}
		if !strings.Contains(board, "boardById.get(t.board_id)") {
			t.Error("the chip name must be resolved via boardById.get(t.board_id)")
		}
		// The chip is conditional on a resolved name — a dangling id => no chip.
		if !strings.Contains(board, "boardName ?") {
			t.Error("the chip must be conditional on a resolved boardName (dangling id => no chip)")
		}
		if !strings.Contains(board, "new Map((boards") {
			t.Error("boardById must be built from the fetched boards (id → name)")
		}
	})

	// AC4 — switching projects resets the selection to 'Tous' and refetches the
	// boards. Pinned: the reset is tied to load()'s resetCycle path (the one that
	// ctx.onSelection triggers via load(true)), and boards are fetched in load().
	t.Run("ProjectSwitchResetsAndRefetches", func(t *testing.T) {
		board := readAsset(t, "static/v2/board.js")
		if !strings.Contains(board, "if (resetCycle) boardSel = 'all'") {
			t.Error("a project switch (resetCycle) must reset boardSel to 'all'")
		}
		if !strings.Contains(board, "ctx.onSelection(() => { if (!root.hidden) load(true)") {
			t.Error("selection changes must call load(true) (the resetCycle path)")
		}
		if !strings.Contains(board, "ctx.api.boards(sel)") {
			t.Error("load() must (re)fetch boards for the selected project")
		}
	})

	// AC5 — v1 (static/index.html) is untouched: none of the v2 board markers may
	// leak into the v1 showroom. Byte-identity of the non-test change (board.js
	// only) is verified out-of-band (git diff --stat main…HEAD). The api.boards
	// route is pre-existing, so no api.js/route change was needed.
	t.Run("V1UntouchedNoRouteAdded", func(t *testing.T) {
		v1 := readAsset(t, "static/index.html")
		for _, marker := range []string{"board-pill", "kcard-board", "renderBoardFilter"} {
			if strings.Contains(v1, marker) {
				t.Errorf("v1 index.html unexpectedly contains %q — v2 board selector leaked into v1", marker)
			}
		}
		// the boards wrapper already existed before this change (no new REST route).
		api := readAsset(t, "static/v2/api.js")
		if !strings.Contains(api, "boards: (project) =>") {
			t.Error("expected the pre-existing boards(project) wrapper in api.js (no new route added)")
		}
	})
}
