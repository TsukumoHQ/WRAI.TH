package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// Referential-integrity scan — Phase 0 (task 536ecc40, design
// trovex/7c0e0b8cc39b41ff8dff486b83695f3d, ruled by cto-tsukumo 2026-09-01).
//
// NOTE: this is distinct from integrity.go, which does STRUCTURAL integrity
// (SQLite PRAGMA integrity_check / foreign_key_check on the file). Here we check
// REFERENTIAL integrity: dangling string-natural-key references between rows.
//
// The relay's whole identity/namespace model rests on STRING natural keys (agent
// name, project name, profile slug) referenced by string across ~34 tables, with
// no enforced FK on those refs. Rows accumulate that point at an identity which
// no longer resolves (a deactivated/deleted agent, an unbacked profile slug) —
// the silent orphan/misroute/limbo class. This pass DETECTS and LOGS those
// dangling references into the integrity_quarantine side-table.
//
// Phase 0 is pure OBSERVABILITY: it changes NOTHING about how a flagged row
// behaves. The task keeps its real status and stays claimable/visible exactly as
// before; a quarantine row is inert metadata a human/CTO (and, later, the ledger
// seam) can read. No enforcement, no reroute, no deletion. Phases 1/2 (separate
// PRs) add reconcile + soft-cascade on top of this surface.
//
// Design decisions (ruled):
//   - Storage is a SIDE-TABLE, never flag columns on tasks/agents, so the
//     taskColumns↔scanTask / agentColumns↔scanAgent lockstep is untouched.
//   - Sentinel principals {linear, cron, user} are VALID non-agent references and
//     are never flagged. Service agents (is_service=1) have real agent rows, so
//     hard-orphan checks resolve them automatically; the limbo check excludes
//     them explicitly (they are exempt from the liveness gate).
//   - A value that equals a profile SLUG but has a matching agent NAME row (the
//     analytics-lead / cro-lead name==slug collision) RESOLVES and is not an
//     orphan — detection asks only "does an agent row exist for (project,value)?",
//     so the name-vs-slug ambiguity never mis-flags a live agent.
//   - profile_slug orphans are STRUCTURAL (the profiles table is sparse; many
//     valid dispatch profiles have no formal profiles row). They are TOLERATED +
//     marked here, NEVER rewritten to an agent name (that would defeat the
//     profile-pool dispatch model).

// integritySentinels are the reserved non-agent principals that legitimately
// have no agents row. Lowercase; matched case-insensitively. Kept a const map
// (deterministic, code-is-law) rather than config; service agents are handled
// dynamically via their is_service row, not by name.
var integritySentinels = map[string]bool{
	"linear": true,
	"cron":   true,
	"user":   true,
}

// sentinelSQLList renders the sentinel set as a SQL literal list for a
// `LOWER(col) NOT IN (...)` guard. Sorted so the generated SQL is deterministic.
func sentinelSQLList() string {
	vals := make([]string, 0, len(integritySentinels))
	for k := range integritySentinels {
		vals = append(vals, "'"+k+"'")
	}
	sort.Strings(vals)
	return strings.Join(vals, ", ")
}

// refCheck is one orphan class. orphanSQL selects the CURRENT offending rows with
// EXACTLY three output columns aliased row_id, ref_value, project. class↔(table,
// refCol) is 1:1, so a class name alone keys a row's quarantine lifecycle.
type refCheck struct {
	class     string
	table     string
	refCol    string
	orphanSQL string
}

// refChecks builds the full check set. It is a function (not a package var)
// because the sentinel guard is interpolated into the SQL.
func refChecks() []refCheck {
	sent := sentinelSQLList()
	return []refCheck{
		// --- task agent-name references (the big orphan counts) ---
		{
			class: "orphan_dispatcher", table: "tasks", refCol: "dispatched_by",
			orphanSQL: `SELECT t.id AS row_id, t.dispatched_by AS ref_value, t.project AS project
				FROM tasks t
				WHERE t.dispatched_by IS NOT NULL AND t.dispatched_by <> ''
				  AND t.archived_at IS NULL
				  AND LOWER(t.dispatched_by) NOT IN (` + sent + `)
				  AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.project = t.project AND LOWER(a.name) = LOWER(t.dispatched_by))`,
		},
		{
			class: "orphan_assignee", table: "tasks", refCol: "assigned_to",
			orphanSQL: `SELECT t.id AS row_id, t.assigned_to AS ref_value, t.project AS project
				FROM tasks t
				WHERE t.assigned_to IS NOT NULL AND t.assigned_to <> ''
				  AND t.archived_at IS NULL
				  AND LOWER(t.assigned_to) NOT IN (` + sent + `)
				  AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.project = t.project AND LOWER(a.name) = LOWER(t.assigned_to))`,
		},
		{
			class: "orphan_claimer", table: "tasks", refCol: "claimed_by",
			orphanSQL: `SELECT t.id AS row_id, t.claimed_by AS ref_value, t.project AS project
				FROM tasks t
				WHERE t.claimed_by IS NOT NULL AND t.claimed_by <> ''
				  AND t.archived_at IS NULL
				  AND LOWER(t.claimed_by) NOT IN (` + sent + `)
				  AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.project = t.project AND LOWER(a.name) = LOWER(t.claimed_by))`,
		},
		// --- limbo: a NON-TERMINAL task assigned to an agent that EXISTS but is
		// not active (deactivated/deleted/sleeping) and is not a service identity.
		// This is the permanent-claim-limbo class the audit called out — the task
		// can never be picked up because its assignee is dead. Distinct from
		// orphan_assignee (no agent row at all).
		{
			class: "limbo", table: "tasks", refCol: "assigned_to",
			orphanSQL: `SELECT t.id AS row_id, t.assigned_to AS ref_value, t.project AS project
				FROM tasks t
				JOIN agents a ON a.project = t.project AND LOWER(a.name) = LOWER(t.assigned_to)
				WHERE t.status NOT IN ('done', 'cancelled')
				  AND t.assigned_to IS NOT NULL AND t.assigned_to <> ''
				  AND t.archived_at IS NULL
				  AND a.status <> 'active'
				  AND a.is_service = 0`,
		},
		// --- task profile_slug: STRUCTURAL orphan (sparse profiles table).
		// Tolerated + marked, never rewritten. Empty slug (Linear mirror inserts '')
		// is not an orphan.
		{
			class: "orphan_profile", table: "tasks", refCol: "profile_slug",
			orphanSQL: `SELECT t.id AS row_id, t.profile_slug AS ref_value, t.project AS project
				FROM tasks t
				WHERE t.profile_slug IS NOT NULL AND t.profile_slug <> ''
				  AND t.archived_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM profiles p WHERE p.project = t.project AND LOWER(p.slug) = LOWER(t.profile_slug))`,
		},
		// --- task id references (uuid) ---
		{
			class: "orphan_parent", table: "tasks", refCol: "parent_task_id",
			orphanSQL: `SELECT t.id AS row_id, t.parent_task_id AS ref_value, t.project AS project
				FROM tasks t
				WHERE t.parent_task_id IS NOT NULL AND t.parent_task_id <> ''
				  AND t.archived_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM tasks p WHERE p.id = t.parent_task_id)`,
		},
		{
			class: "orphan_board", table: "tasks", refCol: "board_id",
			orphanSQL: `SELECT t.id AS row_id, t.board_id AS ref_value, t.project AS project
				FROM tasks t
				WHERE t.board_id IS NOT NULL AND t.board_id <> ''
				  AND t.archived_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM boards b WHERE b.id = t.board_id)`,
		},
		// --- task project reference ---
		{
			class: "orphan_task_project", table: "tasks", refCol: "project",
			orphanSQL: `SELECT t.id AS row_id, t.project AS ref_value, t.project AS project
				FROM tasks t
				WHERE t.project IS NOT NULL AND t.project <> ''
				  AND t.archived_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.name = t.project)`,
		},
		// --- agent self references ---
		{
			class: "orphan_reports_to", table: "agents", refCol: "reports_to",
			orphanSQL: `SELECT a.id AS row_id, a.reports_to AS ref_value, a.project AS project
				FROM agents a
				WHERE a.reports_to IS NOT NULL AND a.reports_to <> ''
				  AND LOWER(a.reports_to) NOT IN (` + sent + `)
				  AND NOT EXISTS (SELECT 1 FROM agents b WHERE b.project = a.project AND LOWER(b.name) = LOWER(a.reports_to))`,
		},
		{
			class: "orphan_agent_profile", table: "agents", refCol: "profile_slug",
			orphanSQL: `SELECT a.id AS row_id, a.profile_slug AS ref_value, a.project AS project
				FROM agents a
				WHERE a.profile_slug IS NOT NULL AND a.profile_slug <> ''
				  AND NOT EXISTS (SELECT 1 FROM profiles p WHERE p.project = a.project AND LOWER(p.slug) = LOWER(a.profile_slug))`,
		},
		// --- message recipient / sender. Broadcast ('*'), team ('team:%') and
		// empty (conversation-scoped) targets are addressing forms, not agent names.
		{
			class: "orphan_recipient", table: "messages", refCol: "to_agent",
			orphanSQL: `SELECT m.id AS row_id, m.to_agent AS ref_value, m.project AS project
				FROM messages m
				WHERE m.to_agent IS NOT NULL AND m.to_agent <> '' AND m.to_agent <> '*'
				  AND m.to_agent NOT LIKE 'team:%'
				  AND LOWER(m.to_agent) NOT IN (` + sent + `)
				  AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.project = m.project AND LOWER(a.name) = LOWER(m.to_agent))`,
		},
		{
			class: "orphan_sender", table: "messages", refCol: "from_agent",
			orphanSQL: `SELECT m.id AS row_id, m.from_agent AS ref_value, m.project AS project
				FROM messages m
				WHERE m.from_agent IS NOT NULL AND m.from_agent <> ''
				  AND LOWER(m.from_agent) NOT IN (` + sent + `)
				  AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.project = m.project AND LOWER(a.name) = LOWER(m.from_agent))`,
		},
	}
}

// referentialScanTimeout bounds the whole scan (all checks share one writer tx).
// A var, not a const, so a test can shrink it.
var referentialScanTimeout = 30 * time.Second

// runReferentialScan runs every referential check inside one writer transaction
// and upserts the results into integrity_quarantine. It returns the count of
// currently-OPEN (unresolved) quarantine rows per class. Idempotent: re-running
// on an unchanged DB inserts nothing and flips no marker. Backward-compatible:
// only the side-table is written; no existing row is touched.
//
// Per check, three steps keep the lifecycle correct and re-run-safe:
//  1. INSERT OR IGNORE the current orphans (new ones get detected_at; the UNIQUE
//     key dedupes existing ones — first-seen detected_at is preserved).
//  2. RE-OPEN any row that had been marked resolved but is orphan again.
//  3. MARK RESOLVED any open row whose ref now resolves (no delete — audit trail).
func runReferentialScan(conn *sql.DB) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), referentialScanTimeout)
	defer cancel()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("referential scan: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after commit

	now := time.Now().UTC().Format(memoryTimeFmt)

	for _, c := range refChecks() {
		// 1. insert new orphans
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO integrity_quarantine
			   (table_name, row_id, ref_col, ref_value, class, project, detected_at)
			 SELECT ?, o.row_id, ?, o.ref_value, ?, o.project, ?
			 FROM (`+c.orphanSQL+`) o`,
			c.table, c.refCol, c.class, now,
		); err != nil {
			return nil, fmt.Errorf("referential scan %s: insert: %w", c.class, err)
		}
		// 2. re-open regressed rows (resolved before, orphan again)
		if _, err := tx.ExecContext(ctx,
			`UPDATE integrity_quarantine
			 SET resolved_at = NULL, detected_at = ?
			 WHERE class = ? AND resolved_at IS NOT NULL
			   AND row_id IN (SELECT o.row_id FROM (`+c.orphanSQL+`) o)`,
			now, c.class,
		); err != nil {
			return nil, fmt.Errorf("referential scan %s: reopen: %w", c.class, err)
		}
		// 3. mark healed rows resolved (ref now resolves) — never deleted
		if _, err := tx.ExecContext(ctx,
			`UPDATE integrity_quarantine
			 SET resolved_at = ?
			 WHERE class = ? AND resolved_at IS NULL
			   AND row_id NOT IN (SELECT o.row_id FROM (`+c.orphanSQL+`) o)`,
			now, c.class,
		); err != nil {
			return nil, fmt.Errorf("referential scan %s: resolve: %w", c.class, err)
		}
	}

	counts := map[string]int{}
	rows, err := tx.QueryContext(ctx,
		`SELECT class, COUNT(*) FROM integrity_quarantine WHERE resolved_at IS NULL GROUP BY class`)
	if err != nil {
		return nil, fmt.Errorf("referential scan: count: %w", err)
	}
	for rows.Next() {
		var class string
		var n int
		if err := rows.Scan(&class, &n); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("referential scan: count scan: %w", err)
		}
		counts[class] = n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("referential scan: count rows: %w", err)
	}
	_ = rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("referential scan: commit: %w", err)
	}
	return counts, nil
}

// NOTE (Phase 0 scope): the scan runs only at startup, inside migrate(), which
// executes on the single writer connection BEFORE the relay serves — so it never
// contends with live writes. A PERIODIC re-scan (to surface orphans created
// during a long uptime without a reboot) is deliberately deferred to Phase 2,
// where it is paired with the reconcile/GC that acts on the markers and can be
// tuned to run off the writer hot path (design §7.3). Adding it to the 5-minute
// StartCleanup loop now would put a heavy multi-full-scan writer transaction on a
// hot path for markers nothing yet consumes.

// logReferentialCounts emits a single deterministic summary line for a scan pass.
// Silent when nothing is open (no noise on a clean DB). Keys are sorted so the
// line is stable across runs.
func logReferentialCounts(phase string, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	classes := make([]string, 0, len(counts))
	total := 0
	for c, n := range counts {
		classes = append(classes, c)
		total += n
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%s=%d", c, counts[c]))
	}
	log.Printf("integrity: %s referential scan — %d open across %d class(es): %s",
		phase, total, len(classes), strings.Join(parts, " "))
}
