package db

import (
	"agent-relay/internal/models"
	"fmt"
	"time"
)

// Limbo sweep (A1-ii, aligned to DEC-wraith-limbo-sweep-rule-1). A task claimed
// by an agent that has since gone inactive stays owned forever: no live agent
// will ever resume it, yet it is not pending so the lease sweep (which only
// requeues an EXPIRED lease of a dead holder to pending) never converges it
// either — it just sits, held by a ghost. This rule closes that class in ONE
// reversible step, on the SAME 2-minute maintenance ticker as the referential
// and lease sweeps:
//
//	BLOCK (blockLimboAfter): the task is BLOCKED with a reason naming the dead
//	    assignee and its dispatcher. Reversible (a blocked task can be resumed
//	    via update_task/resume_task), nothing lost. One digest per dispatcher per
//	    sweep is emitted by the relay layer, and — in apply mode — one audit row
//	    per successful block records the action.
//
// A task is limbo only when ALL THREE hold (DEC-wraith-limbo-sweep-rule-1):
//   - the ASSIGNEE is inactive (SQL prefilter) and both clocks are >7d stale,
//   - the DISPATCHER is itself inactive (row missing OR status not in
//     active/sleeping — no last_seen threshold). A live lead still tracking the
//     task keeps it out of limbo.
//
// The task is NEVER auto-archived and NEVER deleted (the earlier tier-2 archive
// was removed by the ruling), and the assignee is NEVER reassigned
// (DEC-wraith-update-task-reassign-1): a dead agent's work is quarantined, not
// silently handed to someone else or dropped.
//
// Staleness is measured on BOTH clocks — the task's last_activity_at AND the
// agent's last_seen — and both must be past the threshold. created_at alone is
// never used: a freshly-dispatched task to an agent that only just went quiet is
// not limbo yet.
//
// Dry-run is the DEFAULT at first deploy (the relay layer decides `apply`):
// every disposition is computed and journaled (ZERO rows written, including
// audit), so the first real runs can be audited against the expected set before
// anything moves.
const (
	blockLimboAfter = 7 * 24 * time.Hour
	// limboReasonPrefix is the reason we stamp on a limbo block. Idempotence no
	// longer rides on it: the sweep skips ANY already-blocked task (see the block
	// guard), so re-running is a no-op regardless of who set the reason.
	limboReasonPrefix = "limbo-sweep"
)

// LimboDisposition is one task the sweep acted on (or, in dry-run, would act on).
type LimboDisposition struct {
	TaskID       string
	Project      string
	AssignedTo   string
	DispatchedBy string
	FromStatus   string // status before the block (for the CAS + journal)
	AgeDays      int    // whole days since last_activity_at (dry-run shadow line)
	Reason       string // the blocked_reason the block stamps
}

// LimboSweepResult is the outcome of one sweep pass.
type LimboSweepResult struct {
	DryRun  bool
	Scanned int                // rows that passed the cheap SQL filter
	Blocked []LimboDisposition // tasks blocked (or, in dry-run, would-block)
}

// limboRow is a scanned candidate before the Go-side liveness/staleness filters.
type limboRow struct {
	id, project, assignedTo, dispatchedBy, status string
	lastActivityAt, leaseHolder, leaseExpiresAt   string
	blockedReason, blockedPeriods, agentLastSeen  string
}

// SweepLimboAssignees computes and (when apply is true) applies the limbo
// dispositions as of `now`. It is single-writer safe: the read cursor is fully
// drained and closed BEFORE any write, and every write is a CAS-guarded
// writerExec so a concurrent transition can never be clobbered. With apply=false
// it performs zero writes and only reports what it would do (dry-run).
func (d *DB) SweepLimboAssignees(now time.Time, apply bool) (*LimboSweepResult, error) {
	nowStr := now.UTC().Format(memoryTimeFmt)
	blockCut := now.Add(-blockLimboAfter).UTC().Format(memoryTimeFmt)

	// Cheap SQL pre-filter: non-terminal, live (not archived), actually assigned,
	// whose assignee agent row exists and is NOT active and is NOT a service. The
	// (project, LOWER(name)) join mirrors the referential scan's identity key.
	// The finer conditions (live-lease, both-clock staleness, idempotence,
	// dispatcher liveness) are applied per row below.
	rows, err := d.ro().Query(`
		SELECT t.id, t.project, t.assigned_to, t.dispatched_by, t.status,
		       COALESCE(t.last_activity_at, ''), COALESCE(t.lease_holder, ''),
		       COALESCE(t.lease_expires_at, ''), COALESCE(t.blocked_reason, ''),
		       COALESCE(t.blocked_periods, '[]'), COALESCE(a.last_seen, '')
		FROM tasks t
		JOIN agents a ON a.project = t.project AND LOWER(a.name) = LOWER(t.assigned_to)
		WHERE t.status NOT IN ('done', 'cancelled')
		  AND t.archived_at IS NULL
		  AND t.assigned_to <> ''
		  AND a.status <> 'active'
		  AND a.is_service = 0`)
	if err != nil {
		return nil, fmt.Errorf("limbo sweep: query: %w", err)
	}

	var candidates []limboRow
	for rows.Next() {
		var r limboRow
		if err := rows.Scan(&r.id, &r.project, &r.assignedTo, &r.dispatchedBy, &r.status,
			&r.lastActivityAt, &r.leaseHolder, &r.leaseExpiresAt,
			&r.blockedReason, &r.blockedPeriods, &r.agentLastSeen); err != nil {
			rows.Close()
			return nil, fmt.Errorf("limbo sweep: scan: %w", err)
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("limbo sweep: rows: %w", err)
	}
	rows.Close() // drain + close BEFORE any write (single-writer discipline)

	res := &LimboSweepResult{DryRun: !apply, Scanned: len(candidates)}
	// dispatcher liveness is looked up once per distinct dispatcher.
	dispCache := map[string]agentLiveness{}

	for _, r := range candidates {
		// Never touch an already-blocked task: it is out of limbo by definition (a
		// held-by-a-ghost task is non-blocked), and re-blocking would overwrite the
		// original blocked_reason — destroying the provenance of WHY it was blocked
		// (e.g. a human-set reason). This subsumes the limbo-prefix idempotence
		// check: a task blocked by a prior limbo sweep is skipped too, so re-running
		// stays a no-op (no re-block, no blocked_periods churn). Skipped = no shadow
		// line, no block, no audit row.
		if r.status == "blocked" {
			continue
		}
		// A still-live lease means the holder is not really gone (e.g. a sleeping
		// agent whose lease has not lapsed) — not limbo. Mirrors task_lease.go's
		// liveness: expired lease OR dead holder = not live.
		if r.leaseExpiresAt != "" && r.leaseExpiresAt > nowStr && d.agentLive(r.project, r.leaseHolder) {
			continue
		}
		// Both clocks must be past the threshold, and both must be present — a
		// missing timestamp is not evidence of staleness.
		if r.lastActivityAt == "" || r.agentLastSeen == "" {
			continue
		}
		if !(r.lastActivityAt < blockCut && r.agentLastSeen < blockCut) {
			continue
		}

		// The DISPATCHER must ALSO be inactive (DEC-wraith-limbo-sweep-rule-1): a
		// live lead still tracking the task keeps it out of limbo. Inactive = agent
		// row missing OR status not in active/sleeping; NO last_seen threshold on
		// the dispatcher.
		dl, seen := dispCache[r.dispatchedBy]
		if !seen {
			dl = d.lookupAgentLiveness(r.project, r.dispatchedBy)
			dispCache[r.dispatchedBy] = dl
		}
		if dl.exists && dl.active {
			continue // dispatcher active or sleeping → not limbo, keep
		}

		// BLOCK — naming the dead assignee (with last_seen) and its dispatcher.
		reason := fmt.Sprintf("%s: %s (last_seen %s) dispatcher %s",
			limboReasonPrefix, r.assignedTo, r.agentLastSeen, r.dispatchedBy)
		disposition := LimboDisposition{
			TaskID: r.id, Project: r.project, AssignedTo: r.assignedTo,
			DispatchedBy: r.dispatchedBy, FromStatus: r.status,
			AgeDays: limboAgeDays(now, r.lastActivityAt), Reason: reason,
		}
		if apply {
			ok, err := d.blockLimboTask(r.id, r.project, r.status, reason, r.blockedPeriods, nowStr)
			if err != nil {
				return res, fmt.Errorf("limbo sweep: block %s: %w", r.id, err)
			}
			if !ok {
				continue // raced (status changed under us) — CAS no-op
			}
			// One audit row per successful block (apply only; dry-run writes zero,
			// including this). Best-effort: audit failure never blocks the sweep.
			_ = d.RecordAudit(models.AuditEntry{
				Action:       "task.limbo_blocked",
				Actor:        "relay-sweeper",
				Project:      r.project,
				ResourceType: "task",
				ResourceID:   r.id,
				Reason:       reason,
			})
		}
		res.Blocked = append(res.Blocked, disposition)
	}
	return res, nil
}

// limboAgeDays is the whole number of days between a task's last_activity_at and
// now — the age the dry-run shadow line reports. Parsed with time.RFC3339, which
// accepts BOTH fractional-second stamps (memoryTimeFmt's .000000Z) and plain
// second-precision stamps (…T12:19:03Z); the zero-padded memoryTimeFmt layout
// requires exactly 6 fractional digits and so silently mis-parsed no-frac stamps
// to 0. A missing/unparseable timestamp (never expected for a scanned candidate)
// or a future stamp still yields 0. memoryTimeFmt itself is unchanged (other
// callers depend on it) — only this age display is widened.
func limboAgeDays(now time.Time, lastActivityAt string) int {
	ts, err := time.Parse(time.RFC3339, lastActivityAt)
	if err != nil {
		return 0
	}
	days := int(now.UTC().Sub(ts) / (24 * time.Hour))
	if days < 0 {
		return 0
	}
	return days
}

// AgentActive reports whether an agent is a live owner candidate (active or
// sleeping). Exported so the relay layer can gate a limbo digest on the
// dispatcher being alive without duplicating the liveness rule.
func (d *DB) AgentActive(project, name string) bool {
	return d.agentLive(project, name)
}

// agentLiveness is a cached snapshot of one agent's existence/liveness.
type agentLiveness struct {
	exists   bool
	active   bool // active or sleeping (a live owner candidate)
	lastSeen string
}

// lookupAgentLiveness reads an agent row for the dispatcher liveness check. A
// missing row means gone.
func (d *DB) lookupAgentLiveness(project, name string) agentLiveness {
	if name == "" {
		return agentLiveness{}
	}
	var status, lastSeen string
	err := d.ro().QueryRow(
		"SELECT status, COALESCE(last_seen, '') FROM agents WHERE name = ? AND project = ?",
		name, project,
	).Scan(&status, &lastSeen)
	if err != nil {
		return agentLiveness{}
	}
	return agentLiveness{exists: true, active: status == "active" || status == "sleeping", lastSeen: lastSeen}
}

// blockLimboTask performs the block transition as a single-writer CAS. It is the
// "équivalent single-writer" the ruling allows: BlockTask goes through
// validTransitions, which does not permit accepted→blocked, yet an accepted task
// claimed by a dead agent is exactly a limbo case. This writes the same columns
// the blocked branch of transitionTask does (status, blocked_reason, an opened
// blocked_periods window) but admits any non-terminal FROM status. The CAS guard
// (status = fromStatus) makes a concurrent transition win instead of being
// clobbered; RowsAffected==0 → the caller treats it as a raced no-op.
func (d *DB) blockLimboTask(taskID, project, fromStatus, reason, blockedPeriods, now string) (bool, error) {
	bp := openBlockedPeriod(blockedPeriods, now)
	res, err := d.writerExec(
		`UPDATE tasks SET status = 'blocked', blocked_reason = ?, blocked_periods = ?
		 WHERE id = ? AND project = ? AND status = ? AND status NOT IN ('done', 'cancelled')`,
		reason, bp, taskID, project, fromStatus,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
