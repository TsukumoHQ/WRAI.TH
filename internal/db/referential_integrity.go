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
		// is not an orphan. TWO resolvers: a slug is live when a profiles row matches
		// OR any in-project agent carries it (the agents pool is the real registry —
		// DEC-wraith-orphan-profile-burndown-1 Q1, phase3 Q5). Terminal tasks
		// (done/cancelled) are excluded — a dead slug on a finished task is noise, not
		// a limbo (limbo-class precedent above) — DEC-wraith-orphan-profile-burndown-1 Q2.
		{
			class: "orphan_profile", table: "tasks", refCol: "profile_slug",
			orphanSQL: `SELECT t.id AS row_id, t.profile_slug AS ref_value, t.project AS project
				FROM tasks t
				WHERE t.profile_slug IS NOT NULL AND t.profile_slug <> ''
				  AND t.archived_at IS NULL
				  AND t.status NOT IN ('done', 'cancelled')
				  AND NOT EXISTS (SELECT 1 FROM profiles p WHERE p.project = t.project AND LOWER(p.slug) = LOWER(t.profile_slug))
				  AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.project = t.project AND LOWER(a.profile_slug) = LOWER(t.profile_slug))`,
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
		// --- project refs on the 4 tables the Phase 0 scan originally missed
		// (audit 494b6323 observed triggers.project 33, memories.project 3,
		// workflows.project 3, cycles.project 1 — never wired into refChecks()
		// until now; ruled GO in DEC-wraith-referential-integrity-phase3-1 §7-Q3).
		// Same shape as orphan_task_project: no sentinel guard needed (a project
		// name is not a principal, so {linear,cron,user} never appear here). ---
		{
			class: "orphan_trigger_project", table: "triggers", refCol: "project",
			orphanSQL: `SELECT tr.id AS row_id, tr.project AS ref_value, tr.project AS project
				FROM triggers tr
				WHERE tr.project IS NOT NULL AND tr.project <> ''
				  AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.name = tr.project)`,
		},
		{
			class: "orphan_memory_project", table: "memories", refCol: "project",
			orphanSQL: `SELECT m.id AS row_id, m.project AS ref_value, m.project AS project
				FROM memories m
				WHERE m.project IS NOT NULL AND m.project <> ''
				  AND m.archived_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.name = m.project)`,
		},
		{
			class: "orphan_workflow_project", table: "workflows", refCol: "project",
			orphanSQL: `SELECT w.id AS row_id, w.project AS ref_value, w.project AS project
				FROM workflows w
				WHERE w.project IS NOT NULL AND w.project <> ''
				  AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.name = w.project)`,
		},
		{
			class: "orphan_cycle_project", table: "cycles", refCol: "project",
			orphanSQL: `SELECT c.id AS row_id, c.project AS ref_value, c.project AS project
				FROM cycles c
				WHERE c.project IS NOT NULL AND c.project <> ''
				  AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.name = c.project)`,
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

// refScanReadHook and refScanBetweenHook are test-only seams (nil in production):
// refScanReadHook fires once during the read phase (after the first class is read)
// so a test can observe the writer is free while reads are in flight;
// refScanBetweenHook fires in the live path AFTER the read phase and BEFORE the
// apply tx so a test can mutate the DB and exercise the accepted read↔apply race.
var (
	refScanReadHook    func()
	refScanBetweenHook func()
)

// --- scan orchestration -----------------------------------------------------
//
// The scan is split into a READ phase and an APPLY phase so it never holds the
// single writer connection (db.go SetMaxOpenConns(1)) for longer than the
// milliseconds its writes need. Root cause of the 2026-09-02 writer-starvation
// window (task 1ac2ce6e): the previous implementation ran all ~16 classes' five
// statements — each embedding the class orphanSQL — inside ONE writer tx bounded
// by referentialScanTimeout (30s) > writerTimeout (15s), so under fleet load a
// 20–40s scan deadlined every other write. Now the orphanSQL runs ONCE per class
// on the reader pool (read phase), the deltas are computed in Go, and only short
// INSERT/UPDATE-by-row_id statements touch the writer (apply phase, bounded by
// writerTimeout via beginWriterTx). Pattern mirrors dangling_board.go and
// limbo_sweep.go (reader scan, then a short writer tx).
//
// Accepted race (ruled): a ref that heals between the read and the apply is
// resolved one tick (2 min) later, not in this scan. Idempotent.

// orphanRow is one current offender: (row_id, ref_value, project) as the
// orphanSQL aliases them.
type orphanRow struct {
	rowID    string
	refValue string
	project  string
}

// refDelta is the per-class change set computed by the read phase from the
// current orphan set and the existing quarantine rows. The apply phase writes
// exactly these — no orphanSQL text runs on the writer.
type refDelta struct {
	c          refCheck
	newOrphans []orphanRow // orphan AND not currently open (brand-new or resolved-then-regressed)
	reopenIDs  []string    // resolved rows that are orphan again → re-open
	resolveIDs []string    // open rows whose ref now resolves → mark resolved
}

// roQueryer is the read surface the read phase needs: the reader pool in the
// live path, the boot tx at startup. *sql.DB, *sql.Tx and *sql.Conn satisfy it.
type roQueryer interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

// execer is the write surface the apply phase needs. *sql.Tx (boot) and the
// embedded *sql.Tx of a writerTx (live) satisfy it.
type execer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// readRefDeltas computes every class's change set with READ-ONLY queries on rq.
// The orphanSQL runs ONCE per class here (never inside the writer tx). Set
// arithmetic is done in Go: newOrphans = orphan \ open, reopen = resolved ∩
// orphan, resolve = open \ orphan. Iteration order of the orphan/open queries is
// preserved into the returned slices so the detect/heal log lines are stable.
func readRefDeltas(ctx context.Context, rq roQueryer) ([]refDelta, error) {
	out := make([]refDelta, 0, 16)
	for i, c := range refChecks() {
		if i == 0 && refScanReadHook != nil {
			refScanReadHook()
		}
		var orphans []orphanRow
		orphanIDs := map[string]bool{}
		orows, err := rq.QueryContext(ctx, c.orphanSQL)
		if err != nil {
			return nil, fmt.Errorf("referential scan %s: orphan read: %w", c.class, err)
		}
		for orows.Next() {
			var o orphanRow
			if err := orows.Scan(&o.rowID, &o.refValue, &o.project); err != nil {
				_ = orows.Close()
				return nil, fmt.Errorf("referential scan %s: orphan scan: %w", c.class, err)
			}
			orphans = append(orphans, o)
			orphanIDs[o.rowID] = true
		}
		if err := orows.Err(); err != nil {
			_ = orows.Close()
			return nil, fmt.Errorf("referential scan %s: orphan rows: %w", c.class, err)
		}
		_ = orows.Close()

		open, err := readQuarantineIDs(ctx, rq, c.class, false)
		if err != nil {
			return nil, fmt.Errorf("referential scan %s: open read: %w", c.class, err)
		}
		resolved, err := readQuarantineIDs(ctx, rq, c.class, true)
		if err != nil {
			return nil, fmt.Errorf("referential scan %s: resolved read: %w", c.class, err)
		}
		openSet := idSet(open)

		d := refDelta{c: c}
		for _, o := range orphans {
			if !openSet[o.rowID] {
				d.newOrphans = append(d.newOrphans, o) // orphan \ open
			}
		}
		for _, id := range resolved {
			if orphanIDs[id] {
				d.reopenIDs = append(d.reopenIDs, id) // resolved ∩ orphan
			}
		}
		for _, id := range open {
			if !orphanIDs[id] {
				d.resolveIDs = append(d.resolveIDs, id) // open \ orphan
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// readQuarantineIDs returns the row_ids of the quarantine rows for a class,
// either the OPEN set (resolved==false) or the RESOLVED set (resolved==true).
func readQuarantineIDs(ctx context.Context, rq roQueryer, class string, resolved bool) ([]string, error) {
	q := `SELECT row_id FROM integrity_quarantine WHERE class = ? AND resolved_at IS NULL`
	if resolved {
		q = `SELECT row_id FROM integrity_quarantine WHERE class = ? AND resolved_at IS NOT NULL`
	}
	rows, err := rq.QueryContext(ctx, q, class)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func idSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// refApplyChunk bounds each UPDATE ... row_id IN (...) list. SQLite's default
// bound-parameter limit is 999; 500 keeps each statement small and well under it.
const refApplyChunk = 500

// applyRefDeltas writes the change sets on the writer surface x. The ONLY
// statements it runs are INSERT OR IGNORE (explicit values) and UPDATE ...
// WHERE row_id IN (explicit list) against integrity_quarantine — no orphanSQL
// text, so the writer holds the connection for milliseconds, not the length of a
// 16-class scan. Order per class: insert, reopen, resolve (matches the pre-split
// statement order so a mid-scan crash leaves the same intermediate state).
func applyRefDeltas(x execer, deltas []refDelta, now string) error {
	for _, d := range deltas {
		c := d.c
		for _, o := range d.newOrphans {
			if _, err := x.Exec(
				`INSERT OR IGNORE INTO integrity_quarantine
				   (table_name, row_id, ref_col, ref_value, class, project, detected_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				c.table, o.rowID, c.refCol, o.refValue, c.class, o.project, now,
			); err != nil {
				return fmt.Errorf("referential scan %s: insert: %w", c.class, err)
			}
		}
		for _, chunk := range chunkStrings(d.reopenIDs, refApplyChunk) {
			ph, ids := inPlaceholders(chunk)
			args := append([]interface{}{now, c.class}, ids...)
			if _, err := x.Exec(
				`UPDATE integrity_quarantine
				 SET resolved_at = NULL, detected_at = ?
				 WHERE class = ? AND resolved_at IS NOT NULL AND row_id IN (`+ph+`)`,
				args...,
			); err != nil {
				return fmt.Errorf("referential scan %s: reopen: %w", c.class, err)
			}
		}
		for _, chunk := range chunkStrings(d.resolveIDs, refApplyChunk) {
			ph, ids := inPlaceholders(chunk)
			args := append([]interface{}{now, c.class}, ids...)
			if _, err := x.Exec(
				`UPDATE integrity_quarantine
				 SET resolved_at = ?
				 WHERE class = ? AND resolved_at IS NULL AND row_id IN (`+ph+`)`,
				args...,
			); err != nil {
				return fmt.Errorf("referential scan %s: resolve: %w", c.class, err)
			}
		}
	}
	return nil
}

// chunkStrings splits ids into slices of at most size (nil in → nil out).
func chunkStrings(ids []string, size int) [][]string {
	if len(ids) == 0 {
		return nil
	}
	var out [][]string
	for i := 0; i < len(ids); i += size {
		j := i + size
		if j > len(ids) {
			j = len(ids)
		}
		out = append(out, ids[i:j])
	}
	return out
}

// inPlaceholders renders "?, ?, ..." and the matching arg slice for an IN list.
func inPlaceholders(ids []string) (string, []interface{}) {
	ph := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return strings.Join(ph, ", "), args
}

// refScanLogLines builds the transition log lines from the deltas: one detect
// line per newly-opening orphan (class order, then orphan-query order), then one
// heal line per resolving row (class order, then open-query order). Byte-identical
// to the pre-split lines; emitted post-commit by the callers.
func refScanLogLines(deltas []refDelta, now string) (detect, heal []string) {
	for _, d := range deltas {
		ref := d.c.table + "." + d.c.refCol
		for _, o := range d.newOrphans {
			detect = append(detect, fmt.Sprintf(
				"integrity: detect class=%s ref=%s value=%s row=%s", d.c.class, ref, o.refValue, o.rowID))
		}
	}
	for _, d := range deltas {
		for _, id := range d.resolveIDs {
			heal = append(heal, fmt.Sprintf(
				"integrity: heal class=%s row=%s action=ref_resolved resolved_at=%s", d.c.class, id, now))
		}
	}
	return detect, heal
}

// openCountsByClass returns the open (unresolved) quarantine count per class,
// the scan's return value.
func openCountsByClass(ctx context.Context, rq roQueryer) (map[string]int, error) {
	counts := map[string]int{}
	rows, err := rq.QueryContext(ctx,
		`SELECT class, COUNT(*) FROM integrity_quarantine WHERE resolved_at IS NULL GROUP BY class`)
	if err != nil {
		return nil, fmt.Errorf("referential scan: count: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var class string
		var n int
		if err := rows.Scan(&class, &n); err != nil {
			return nil, fmt.Errorf("referential scan: count scan: %w", err)
		}
		counts[class] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("referential scan: count rows: %w", err)
	}
	return counts, nil
}

// runReferentialScan runs the scan inside ONE transaction on conn (both read and
// apply). Kept for the boot-time callers (migrate, reconcile) that run before the
// reader pool exists and under no fleet contention — there the single-tx shape is
// harmless. The live periodic path uses (*DB).RunReferentialScan, which splits
// the phases across the reader pool and a short writer tx. Both share
// readRefDeltas/applyRefDeltas, so the SQL and the delta arithmetic are one
// source. Idempotent; only the side-table is written.
func runReferentialScan(conn *sql.DB) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), referentialScanTimeout)
	defer cancel()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("referential scan: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after commit

	now := time.Now().UTC().Format(memoryTimeFmt)

	deltas, err := readRefDeltas(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := applyRefDeltas(tx, deltas, now); err != nil {
		return nil, err
	}
	counts, err := openCountsByClass(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("referential scan: commit: %w", err)
	}

	detect, heal := refScanLogLines(deltas, now)
	for _, l := range detect {
		log.Print(l)
	}
	for _, l := range heal {
		log.Print(l)
	}
	return counts, nil
}

// RunReferentialScan runs the referential scan on the LIVE relay: the read phase
// (every class's orphanSQL) on the reader pool, then ONE short writer tx
// (beginWriterTx, writerTimeout-bounded) that applies only the INSERT/UPDATE
// deltas. The writer is therefore never held for the length of the scan, so a
// slow scan under fleet load can no longer starve send_message/claim_task/
// complete_task/flush/expire into writerTimeout (task 1ac2ce6e). Idempotent.
func (d *DB) RunReferentialScan() (map[string]int, error) {
	// Read phase — reader pool, own ctx (referentialScanTimeout). Holds no writer.
	rctx, rcancel := context.WithTimeout(context.Background(), referentialScanTimeout)
	deltas, err := readRefDeltas(rctx, d.ro())
	rcancel()
	if err != nil {
		return nil, err
	}

	if refScanBetweenHook != nil {
		refScanBetweenHook()
	}

	now := time.Now().UTC().Format(memoryTimeFmt)

	// Apply phase — one short writer tx, bounded by writerTimeout via beginWriterTx.
	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, fmt.Errorf("referential scan: begin writer: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after commit
	if err := applyRefDeltas(tx.Tx, deltas, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("referential scan: commit: %w", err)
	}

	// Count phase — reader pool, post-commit (sees the committed apply).
	cctx, ccancel := context.WithTimeout(context.Background(), referentialScanTimeout)
	counts, err := openCountsByClass(cctx, d.ro())
	ccancel()
	if err != nil {
		return nil, err
	}

	detect, heal := refScanLogLines(deltas, now)
	for _, l := range detect {
		log.Print(l)
	}
	for _, l := range heal {
		log.Print(l)
	}
	return counts, nil
}

// MarkQuarantine upserts ONE referential-integrity quarantine row (the same
// side-table the scan writes). Phase 2 uses it for on-write soft-marking (a ref
// chokepoint that stores a value which does not resolve) and for the
// soft-cascade (a deactivated agent's still-assigned tasks). It never rejects,
// never deletes, never changes the referenced row's behavior — it only records
// the dangling ref so it is visible immediately instead of only at the next
// scan. Idempotent via the UNIQUE(table_name,row_id,ref_col,class) key; a row
// already present (open) is left as-is (first-seen detected_at preserved).
func (d *DB) MarkQuarantine(table, rowID, refCol, refValue, class, project string) error {
	now := time.Now().UTC().Format(memoryTimeFmt)
	res, err := d.writerExec(
		`INSERT OR IGNORE INTO integrity_quarantine
		   (table_name, row_id, ref_col, ref_value, class, project, detected_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		table, rowID, refCol, refValue, class, project, now,
	)
	if err != nil {
		return fmt.Errorf("mark quarantine %s/%s: %w", class, rowID, err)
	}
	// One detect line per actual insert (RowsAffected==0 = idempotent dedup of an
	// already-open row, which must not re-log). Mirrors the scan's detect line so
	// `grep 'integrity: detect'` on the live log covers both the batch scan and
	// this on-write soft-mark path.
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("integrity: detect class=%s ref=%s value=%s row=%s",
			class, table+"."+refCol, refValue, rowID)
	}
	return nil
}

// refResolvesToLiveAgent reports whether name is a valid principal for a
// *_by/assignee reference in project: a sentinel ({linear,cron,user}) or an
// agent row that exists and is not soft-deleted. Used by the on-write soft-mark
// and the soft-cascade to decide whether a ref dangles. Blank/'*'/team: targets
// are the caller's responsibility to pre-filter — this is the agent-name check.
func (d *DB) refResolvesToLiveAgent(project, name string) bool {
	if name == "" {
		return true // nothing to validate
	}
	if integritySentinels[strings.ToLower(strings.TrimSpace(name))] {
		return true
	}
	var n int
	// A tombstoned ('deleted') row does not count as resolving — the ref is
	// effectively dangling. 'inactive'/'sleeping' DO resolve (the agent exists and
	// can come back); the limbo class, not the orphan class, covers dead assignees.
	if err := d.ro().QueryRow(
		`SELECT COUNT(*) FROM agents WHERE project = ? AND LOWER(name) = LOWER(?) AND status != 'deleted'`,
		project, name,
	).Scan(&n); err != nil {
		return true // on a lookup error, do NOT mark (fail open — never over-flag)
	}
	return n > 0
}

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
