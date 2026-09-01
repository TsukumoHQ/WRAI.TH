package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
)

// Referential-integrity reconcile — Phase 1 (task 536ecc40, design §6, ruled by
// cto-tsukumo 2026-09-01). Builds on the Phase 0 detection surface
// (integrity_quarantine): it HEALS the mechanically-resolvable orphans and
// leaves the rest MARKED (never deleted, never rerouted).
//
// It is a ONE-SHOT, settings-marker guarded, snapshot-backed pass. It runs from
// New() (not migrate()) because the pre-mutation snapshot needs *DB. Each rule is
// also independently idempotent (an UPDATE whose WHERE stops matching once the
// row is healed), so a crash mid-pass is safe — the unset marker just retries the
// whole idempotent pass on the next boot.
//
// Two reconcile rules (ruled — nothing else is auto-changed):
//
//	Rule A — sync a stale task.profile_slug to its LIVE assignee's own profile
//	  (the existing backfillTaskProfileSlugs T3 rule, reused). Only touches a task
//	  whose assignee resolves and carries a different, non-empty profile_slug.
//
//	Rule B — an ORPHAN task.assigned_to whose value is a profile SLUG with EXACTLY
//	  ONE active agent of that profile in the project → resolve it to that agent's
//	  NAME. This fixes the name-vs-slug misroute (the audit's analytics-lead class)
//	  by making the task claimable by the one live agent that runs the profile.
//	  More than one candidate, or none, → left unresolved (marked, not touched).
//
// NOT reconciled (left marked as unresolvable, per ruling):
//   - profile_slug orphans that have no backed profile even via the assignee —
//     STRUCTURAL, tolerated, NEVER rewritten to an agent name (Q5: profile_slug is
//     the profile-pool key).
//   - dispatched_by / claimed_by orphans — a historical record of who dispatched/
//     claimed; resolving them to a different agent would fabricate history.
//   - limbo (a non-terminal task whose assignee EXISTS but is dead) — reassigning
//     it to another agent would be a REROUTE, which the ruling forbids; it stays
//     surfaced for human/CTO triage.
//   - sentinels {linear, cron, user} — valid non-agent principals, never touched.

// reconcileOrphansMarker is the one-shot settings guard for the Phase 1 pass.
const reconcileOrphansMarker = "reconcile_orphans_v1"

// reconcileSnapshotSuffix names the dedicated pre-reconcile snapshot. It is NOT
// part of the rotating hourly-backup set, so it survives as the data-rollback
// escape hatch for this specific migration.
const reconcileSnapshotSuffix = ".pre-reconcile-v1.bak"

// maybeReconcileOrphans runs the Phase 1 reconcile once (guarded by
// reconcileOrphansMarker), after taking a pre-mutation snapshot. It is
// non-fatal: any failure logs and leaves the marker UNSET so the next boot
// retries — booting the relay never depends on the reconcile succeeding.
func (d *DB) maybeReconcileOrphans() {
	if d.GetSetting(reconcileOrphansMarker) != "" {
		return // already reconciled on a prior boot
	}

	// Snapshot BEFORE any mutation. If it fails, do NOT reconcile and do NOT set
	// the marker — never mutate data without a rollback point.
	snap, err := d.snapshotBeforeReconcile()
	if err != nil {
		log.Printf("integrity: Phase 1 reconcile SKIPPED — snapshot failed (no mutation done): %v", err)
		return
	}

	// Rule A: sync stale task.profile_slug to the live assignee's own profile.
	synced, err := backfillTaskProfileSlugs(d.conn)
	if err != nil {
		log.Printf("integrity: Phase 1 reconcile profile_slug sync error (marker left unset, will retry): %v", err)
		return
	}

	// Rule B: resolve an orphan assignee that is a profile slug with a sole live
	// agent to that agent's name.
	resolved, err := reconcileAssigneeSoleAgent(d.conn)
	if err != nil {
		log.Printf("integrity: Phase 1 reconcile assignee error (marker left unset, will retry): %v", err)
		return
	}

	// Re-scan so the quarantine markers for the now-healed refs are stamped
	// resolved (audit trail). A rescan failure is not fatal — the DATA reconcile
	// already succeeded; the markers refresh on the next boot's startup scan.
	if _, err := runReferentialScan(d.conn); err != nil {
		log.Printf("integrity: Phase 1 post-reconcile rescan error (non-fatal): %v", err)
	}

	d.SetSetting(reconcileOrphansMarker, "done")
	log.Printf("integrity: Phase 1 reconcile complete — profile_slug synced=%d, assignee resolved=%d (snapshot %s); unresolvable orphans left marked, none deleted",
		synced, resolved, snap)
}

// snapshotBeforeReconcile copies the live DB to a dedicated, non-rotating
// snapshot via VACUUM INTO on a throwaway read-only connection (never the writer
// pool — mirrors Backup()'s discipline so the copy can never wedge live writes).
func (d *DB) snapshotBeforeReconcile() (string, error) {
	dst := d.path + reconcileSnapshotSuffix
	_ = os.Remove(dst) // VACUUM INTO fails if the target already exists

	backupConn, err := sql.Open("sqlite3", d.path+"?_busy_timeout=10000&mode=ro")
	if err != nil {
		return "", fmt.Errorf("open snapshot connection: %w", err)
	}
	defer func() { _ = backupConn.Close() }()
	backupConn.SetMaxOpenConns(1)

	if _, err := backupConn.Exec("VACUUM INTO ?", dst); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", dst, err)
	}
	return dst, nil
}

// reconcileAssigneeSoleAgent implements Rule B (see file header). It resolves an
// orphan task.assigned_to — a value that is a profile SLUG with no agent by that
// name — to the NAME of the sole active agent running that profile in the same
// project. The COUNT(*) = 1 guard makes it unambiguous; the NOT EXISTS guard
// makes it idempotent (once healed, assigned_to names a real agent and no longer
// matches). Returns the number of rows resolved.
func reconcileAssigneeSoleAgent(conn *sql.DB) (int, error) {
	res, err := conn.Exec(`
		UPDATE tasks
		SET assigned_to = (
			SELECT a.name FROM agents a
			WHERE a.project = tasks.project
			  AND LOWER(a.profile_slug) = LOWER(tasks.assigned_to)
			  AND a.status = 'active'
			LIMIT 1
		)
		WHERE assigned_to IS NOT NULL AND assigned_to <> ''
		  AND archived_at IS NULL
		  AND LOWER(assigned_to) NOT IN (` + sentinelSQLList() + `)
		  AND NOT EXISTS (
			SELECT 1 FROM agents a2
			WHERE a2.project = tasks.project AND LOWER(a2.name) = LOWER(tasks.assigned_to)
		  )
		  AND (
			SELECT COUNT(*) FROM agents a3
			WHERE a3.project = tasks.project
			  AND LOWER(a3.profile_slug) = LOWER(tasks.assigned_to)
			  AND a3.status = 'active'
		  ) = 1`)
	if err != nil {
		return 0, fmt.Errorf("reconcile assignee sole-agent: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
