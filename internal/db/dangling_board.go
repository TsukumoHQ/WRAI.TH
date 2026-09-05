package db

import "fmt"

// Dangling board_id sweep (Q4 residual of DEC-wraith-boards-linear-guard-1). A
// native task can keep a board_id that points at a board which no longer exists
// (hard-deleted) or has since been archived. Nothing converges it: the board
// guards refuse deletes/archives that would strand a LINEAR task, but a NATIVE
// task's dangling pointer is only tolerated, never repaired — so the task shows
// under no live board. This sweep re-homes each such task onto its profile's
// product board (ProductBoardSlugForProfile — the SAME lookup the dispatch-time
// auto-router and the backfill use, so the mapping cannot drift), reversibly and
// with a journal trail.
//
// It NEVER deletes a task and NEVER blanks a board_id: a task whose target
// product board does not exist in its project is left exactly as is with a
// 'no-target' disposition. source='linear' rows are never touched — reconcile
// owns their board_id. Archived tasks (archived_at set) are out of scope.
//
// Dry-run is the DEFAULT (the relay layer decides `apply`): every disposition is
// computed and journaled, but ZERO rows are written, so the first real runs can
// be audited before anything moves. Single-writer safe: the candidate cursor and
// every target lookup complete on the read pool BEFORE a single writer tx applies
// all re-homes. Idempotent: after an apply pass every task points at an active
// board, so a re-run finds no candidates.

// DanglingDisposition is one native task the sweep re-homed (or, in dry-run,
// would re-home) because its board_id references a missing or archived board.
type DanglingDisposition struct {
	Task      string // task id
	Project   string
	FromBoard string // the dangling board_id it pointed at
	ToBoard   string // the resolved active product board id ("" when no-target)
	Slug      string // ProductBoardSlugForProfile(profile_slug)
	Action    string // "rehome" | "no-target"
}

// DanglingSweepResult is the outcome of one sweep pass.
type DanglingSweepResult struct {
	DryRun       bool
	Scanned      int // native tasks whose board_id is missing-or-archived
	Dispositions []DanglingDisposition
}

// danglingCandidate is a scanned row before target resolution.
type danglingCandidate struct {
	id, project, profileSlug, boardID string
}

// SweepDanglingBoards computes and (when apply is true) applies the dangling
// board re-homes. With apply=false it performs zero writes and only reports what
// it would do (dry-run).
func (d *DB) SweepDanglingBoards(apply bool) (*DanglingSweepResult, error) {
	// Candidates: a native, non-archived task with a non-empty board_id that
	// resolves to no board (missing) or to an archived board. `board_id <> ''`
	// also excludes NULL (NULL <> '' is NULL/false in SQLite), so an unassigned
	// task is never a candidate. Any task status is in scope — done rows too, so
	// board views stay coherent. source='linear' is excluded (reconcile owns it).
	rows, err := d.ro().Query(`
		SELECT t.id, t.project, t.profile_slug, t.board_id
		FROM tasks t
		LEFT JOIN boards b ON b.id = t.board_id
		WHERE t.source = 'native'
		  AND t.archived_at IS NULL
		  AND t.board_id <> ''
		  AND (b.id IS NULL OR b.archived_at IS NOT NULL)`)
	if err != nil {
		return nil, fmt.Errorf("dangling sweep: query: %w", err)
	}
	var candidates []danglingCandidate
	for rows.Next() {
		var c danglingCandidate
		if err := rows.Scan(&c.id, &c.project, &c.profileSlug, &c.boardID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("dangling sweep: scan: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("dangling sweep: rows: %w", err)
	}
	rows.Close() // drain + close BEFORE any write (single-writer discipline)

	res := &DanglingSweepResult{DryRun: !apply, Scanned: len(candidates)}

	// Resolve the active product board per (project, slug) once — cached, negative
	// results included. All reads happen here, before the writer tx opens.
	type tkey struct{ project, slug string }
	type target struct {
		id string
		ok bool
	}
	targetCache := map[tkey]target{}
	resolve := func(project, slug string) target {
		k := tkey{project, slug}
		if t, seen := targetCache[k]; seen {
			return t
		}
		var id string
		err := d.ro().QueryRow(
			`SELECT id FROM boards WHERE project = ? AND slug = ? AND archived_at IS NULL`,
			project, slug,
		).Scan(&id)
		t := target{}
		if err == nil {
			t = target{id: id, ok: true}
		}
		targetCache[k] = t
		return t
	}

	for _, c := range candidates {
		slug := ProductBoardSlugForProfile(c.profileSlug)
		disp := DanglingDisposition{Task: c.id, Project: c.project, FromBoard: c.boardID, Slug: slug}
		tgt := resolve(c.project, slug)
		if !tgt.ok {
			// No active product board for this profile in this project — leave the
			// task exactly as is (never delete, never blank the board_id).
			disp.Action = "no-target"
			res.Dispositions = append(res.Dispositions, disp)
			continue
		}
		if tgt.id == c.boardID {
			// Impossible for a candidate (an active board can't be missing/archived);
			// guarded anyway — a no-op move is not a disposition.
			continue
		}
		disp.Action = "rehome"
		disp.ToBoard = tgt.id
		res.Dispositions = append(res.Dispositions, disp)
	}

	if !apply {
		return res, nil
	}

	// Apply every re-home in ONE writer tx. The board_id = FromBoard guard makes a
	// concurrent move win instead of being clobbered; RowsAffected 0 is a benign
	// raced no-op. no-target dispositions write nothing.
	tx, err := d.beginWriterTx()
	if err != nil {
		return res, fmt.Errorf("dangling sweep: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, disp := range res.Dispositions {
		if disp.Action != "rehome" {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE tasks SET board_id = ?
			 WHERE id = ? AND project = ? AND board_id = ?
			   AND source = 'native' AND archived_at IS NULL`,
			disp.ToBoard, disp.Task, disp.Project, disp.FromBoard,
		); err != nil {
			return res, fmt.Errorf("dangling sweep: rehome %s: %w", disp.Task, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("dangling sweep: commit: %w", err)
	}
	return res, nil
}
