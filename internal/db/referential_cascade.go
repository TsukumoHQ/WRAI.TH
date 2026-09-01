package db

import (
	"fmt"
	"time"

	"agent-relay/internal/models"
)

// Referential-integrity soft-cascade — Phase 2 (task 536ecc40, design §7.2, ruled
// by cto-tsukumo). When an agent is deactivated or deleted, its referencing rows
// used to dangle silently (DeactivateAgent / DeleteAgent are status flips with no
// cascade) — the source of the permanent claim-limbo the audit found. This adds a
// SOFT cascade: it frees what is safely freeable and MARKS the rest, but NEVER
// hard-deletes a referencing row and NEVER reroutes work to a different agent.
//
//	1. Leased, non-terminal tasks the agent still HOLDS -> released to 'pending'
//	   (assignee + lease cleared), exactly as the expired-lease sweep does for a
//	   dead holder — but immediately, instead of waiting out the lease TTL. The
//	   caller re-emits/nudges so a live agent of the profile re-claims. CAS-guarded
//	   per task, so a live claim that raced in is never clobbered.
//	2. Non-leased, non-terminal tasks merely ASSIGNED to the agent -> MARKED limbo
//	   in the quarantine side-table (surfaced for triage), NOT transitioned: an
//	   assigned-but-unleased task may be intentionally parked, so rerouting it is
//	   out of scope (ruling).
//	3. The agent's team + conversation memberships -> soft-closed (left_at set),
//	   never row-deleted.

// AgentCascade summarizes a soft-cascade so the caller can emit/nudge for the
// released tasks and log the rest.
type AgentCascade struct {
	Released    []SweptLease // leased tasks released to pending (caller emits + nudges)
	MarkedLimbo int          // non-leased assignments marked limbo (surfaced, not moved)
	LeftTeams   int          // team memberships soft-closed
	LeftConvos  int          // conversation memberships soft-closed
}

// CascadeAgentDeactivation runs the soft-cascade for an agent that has just been
// deactivated or deleted (status already flipped by the caller). Best-effort and
// non-fatal per step, mirroring SweepExpiredLeases: a single failed release/mark
// is skipped, the rest proceed, and the periodic sweeps remain the backstop. No
// step ever deletes a row or moves a task to a different agent.
func (d *DB) CascadeAgentDeactivation(project, name string) (*AgentCascade, error) {
	out := &AgentCascade{}
	now := time.Now().UTC().Format(memoryTimeFmt)

	// 1. Release the agent's LEASED, non-terminal tasks -> pending (CAS-guarded).
	rows, err := d.ro().Query(
		`SELECT id, project, title, COALESCE(lease_holder,''), status, priority, profile_slug
		 FROM tasks
		 WHERE project = ? AND LOWER(lease_holder) = LOWER(?)
		   AND status IN ('accepted','in-progress','in-review')
		   AND archived_at IS NULL`,
		project, name,
	)
	if err != nil {
		return nil, fmt.Errorf("cascade: list leased: %w", err)
	}
	type cand struct{ id, project, title, holder, status, priority, profile string }
	var leased []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.project, &c.title, &c.holder, &c.status, &c.priority, &c.profile); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("cascade: scan leased: %w", err)
		}
		leased = append(leased, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("cascade: rows leased: %w", err)
	}
	_ = rows.Close()

	for _, c := range leased {
		res, err := d.writerExec(
			`UPDATE tasks SET status='pending', assigned_to=NULL, lease_holder=NULL,
			   lease_expires_at=NULL, lease_heartbeat_at=NULL, last_activity_at=?
			 WHERE id=? AND project=? AND COALESCE(lease_holder,'')=? AND status=?`,
			now, c.id, c.project, c.holder, c.status,
		)
		if err != nil {
			continue // best-effort; the lease sweep is the backstop
		}
		if n, raErr := res.RowsAffected(); raErr != nil || n == 0 {
			continue // lost a race to a live claim/transition — correct to skip
		}
		d.auditLeaseTransfer(c.project, c.id, "relay-cascade",
			&models.LeaseTransfer{From: c.holder, To: "", Reason: "agent-deactivated"})
		out.Released = append(out.Released, SweptLease{
			TaskID: c.id, Project: c.project, Title: c.title,
			From: c.holder, Priority: c.priority, Profile: c.profile,
		})
	}

	// 2. MARK non-leased, non-terminal tasks ASSIGNED to the agent as limbo —
	// surfaced for triage, never transitioned (rerouting is out of scope).
	arows, err := d.ro().Query(
		`SELECT id, assigned_to FROM tasks
		 WHERE project = ? AND LOWER(assigned_to) = LOWER(?)
		   AND COALESCE(lease_holder,'') = ''
		   AND status NOT IN ('done','cancelled')
		   AND archived_at IS NULL`,
		project, name,
	)
	if err != nil {
		return nil, fmt.Errorf("cascade: list assigned: %w", err)
	}
	type asg struct{ id, assignee string }
	var assigned []asg
	for arows.Next() {
		var a asg
		if err := arows.Scan(&a.id, &a.assignee); err != nil {
			_ = arows.Close()
			return nil, fmt.Errorf("cascade: scan assigned: %w", err)
		}
		assigned = append(assigned, a)
	}
	if err := arows.Err(); err != nil {
		_ = arows.Close()
		return nil, fmt.Errorf("cascade: rows assigned: %w", err)
	}
	_ = arows.Close()

	for _, a := range assigned {
		if err := d.MarkQuarantine("tasks", a.id, "assigned_to", a.assignee, "limbo", project); err == nil {
			out.MarkedLimbo++
		}
	}

	// 3. Soft-close memberships (left_at), never row-delete. conversation_members
	// carries no project column, so scope through the conversation's project.
	if res, err := d.writerExec(
		`UPDATE team_members SET left_at = ?
		 WHERE project = ? AND LOWER(agent_name) = LOWER(?) AND left_at IS NULL`,
		now, project, name,
	); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			out.LeftTeams = int(n)
		}
	}
	if res, err := d.writerExec(
		`UPDATE conversation_members SET left_at = ?
		 WHERE LOWER(agent_name) = LOWER(?) AND left_at IS NULL
		   AND conversation_id IN (SELECT id FROM conversations WHERE project = ?)`,
		now, name, project,
	); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			out.LeftConvos = int(n)
		}
	}

	return out, nil
}
