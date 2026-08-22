package db

import (
	"database/sql"
	"log"
	"strings"
)

// niwaLeadProfiles are the niwa-fleet lead profiles that don't share a common
// "niwa-" prefix (unlike niwa-cto, which does) but still belong on the Niwa
// product board. Kept as an explicit set rather than a prefix rule since
// their names are historical and don't follow a pattern.
var niwaLeadProfiles = map[string]bool{
	"backend-lead":   true,
	"backend-lead-2": true,
	"gate-lead":      true,
	"frontend-lead":  true,
	"cli-lead":       true,
	"niwa-cto":       true,
}

// ProductBoardSlugForProfile is the single source of truth for "which product
// board does this profile belong on" — both the dispatch-time auto-router
// (DispatchTask) and the one-time backfill (backfillProductBoardRouting) funnel
// through it, so the mapping cannot drift between the two.
//
// A profile that matches no known product routes to "backlog" — never a
// refusal — so an unrecognized profile still dispatches; it just doesn't get a
// crisp home the way a monogamous e.g. wraith-* profile does.
func ProductBoardSlugForProfile(profile string) string {
	switch {
	case profile == "yoru" || strings.HasPrefix(profile, "yoru-"):
		return "yoru"
	case profile == "wraith" || strings.HasPrefix(profile, "wraith-"):
		return "wraith"
	case profile == "trovex" || strings.HasPrefix(profile, "trovex-"):
		return "trovex"
	case profile == "niwa" || strings.HasPrefix(profile, "niwa-") || niwaLeadProfiles[profile]:
		return "niwa"
	default:
		return "backlog"
	}
}

// backfillProductBoardRouting is the one-time data migration for the
// auto-routing above: it moves every existing ACTIVE (non-done, non-cancelled)
// task whose board doesn't match its profile's product board — per
// ProductBoardSlugForProfile — onto the correct one, project by project.
// A task is left alone when the target board doesn't exist in its project
// (e.g. a project with no "niwa" board): never invents boards here.
//
// Idempotent: once every task sits on its mapped board, the UPDATE matches
// nothing on a re-run. Returns per-slug counts moved for the caller to log.
func backfillProductBoardRouting(conn *sql.DB) (map[string]int, error) {
	rows, err := conn.Query(`
		SELECT t.id, t.project, t.profile_slug, b.slug
		FROM tasks t
		LEFT JOIN boards b ON b.id = t.board_id
		WHERE t.status NOT IN ('done', 'cancelled')`)
	if err != nil {
		return nil, err
	}
	type move struct {
		taskID, project, wantSlug string
	}
	var moves []move
	for rows.Next() {
		var taskID, project, profileSlug string
		var currentSlug sql.NullString
		if err := rows.Scan(&taskID, &project, &profileSlug, &currentSlug); err != nil {
			_ = rows.Close()
			return nil, err
		}
		want := ProductBoardSlugForProfile(profileSlug)
		if currentSlug.String != want {
			moves = append(moves, move{taskID: taskID, project: project, wantSlug: want})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()

	counts := map[string]int{}
	for _, m := range moves {
		var boardID string
		err := conn.QueryRow(`SELECT id FROM boards WHERE project = ? AND slug = ? AND archived_at IS NULL`,
			m.project, m.wantSlug).Scan(&boardID)
		if err == sql.ErrNoRows {
			continue // no matching board in this project — leave the task where it is
		}
		if err != nil {
			return counts, err
		}
		if _, err := conn.Exec(`UPDATE tasks SET board_id = ? WHERE id = ?`, boardID, m.taskID); err != nil {
			return counts, err
		}
		counts[m.wantSlug]++
	}
	return counts, nil
}

// runProductBoardRoutingBackfill runs backfillProductBoardRouting once (guarded
// by a settings marker, same pattern as backfill_task_profile_slug) and logs
// per-board counts moved.
func runProductBoardRoutingBackfill(conn *sql.DB) {
	var done string
	_ = conn.QueryRow("SELECT value FROM settings WHERE key = 'backfill_product_board_routing'").Scan(&done)
	if done != "" {
		return
	}
	if counts, err := backfillProductBoardRouting(conn); err == nil {
		total := 0
		for slug, n := range counts {
			total += n
			log.Printf("migrate: backfilled %d task(s) onto the %q product board", n, slug)
		}
		if total == 0 {
			log.Printf("migrate: product board routing backfill — no mis-boarded active tasks found")
		}
	} else {
		log.Printf("migrate: product board routing backfill failed: %v", err)
	}
	_, _ = conn.Exec("INSERT INTO settings (key, value) VALUES ('backfill_product_board_routing', 'done') ON CONFLICT(key) DO UPDATE SET value = 'done'")
}
