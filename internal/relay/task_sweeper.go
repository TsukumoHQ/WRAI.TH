package relay

import (
	"log"
	"time"
)

// Task maintenance sweeper (G3/G4). Two relay-INTERNAL periodic sweeps that make
// task convergence self-healing with no external poller:
//
//	G3 — PR-reconcile sweep: a task whose linked PR already reached a terminal
//	     state (pr_state merged/closed, persisted by a received webhook or poll
//	     write-back) but whose task never converged strands forever, because the
//	     open-only reconcile candidate set (ListPRReconcileCandidates) can't see
//	     it. The sweep converges it off the ALREADY-PERSISTED pr_state via the
//	     same prTargetFromState map + no-resurrect ForcePRTransition the webhook
//	     and poll paths use — no gh call, the relay stays inbound-only.
//
//	G4 — lease sweep: a dead reviewer/owner's expired lease is auto-recovered
//	     (SweepExpiredLeases requeues the task to pending), so a stuck task
//	     returns to the claimable pool instead of waiting on an external reclaim.
//
// Both are idempotent and no-resurrect (see the DB methods). One goroutine, one
// ticker; stops when done is closed.
const taskSweepInterval = 2 * time.Minute

// StartTaskMaintenanceSweeper launches the G3/G4 sweeper goroutine.
func (r *Relay) StartTaskMaintenanceSweeper(done <-chan struct{}) {
	if r.DB == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(taskSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.sweepStrandedPRTasks()
				r.sweepExpiredLeases()
			}
		}
	}()
}

// sweepStrandedPRTasks (G3) converges tasks whose PR is merged/closed but whose
// task never transitioned. Uses the SAME status map + no-resurrect transition as
// the webhook and poll convergence paths, so all three agree.
func (r *Relay) sweepStrandedPRTasks() {
	tasks, err := r.DB.ListStrandedPRTasks(500)
	if err != nil {
		log.Printf("pr-reconcile sweep: list: %v", err)
		return
	}
	converged := 0
	for _, t := range tasks {
		if t.PRState == nil {
			continue
		}
		prState := *t.PRState
		target, reason := prTargetFromState(prState)
		if target == "" {
			continue
		}
		var reasonPtr *string
		if reason != "" {
			reasonPtr = &reason
		}
		task, changed, err := r.DB.ForcePRTransition(t.Project, t.ID, target, reasonPtr)
		if err != nil {
			log.Printf("pr-reconcile sweep: converge %s: %v", t.ID, err)
			continue
		}
		if !changed || task == nil {
			continue // already at target / no-resurrect no-op
		}
		converged++
		name := "task.pr_synced"
		if target == "done" && prState == "merged" {
			name = "task.pr_merged"
		}
		r.Events.Emit(MCPEvent{Type: "task", Action: "reconcile_pr", Agent: "relay-sweeper", Project: t.Project, Label: task.Title})
		emitTaskEvent(r.Events, name, "reconcile_pr", t.Project, task, map[string]any{
			"pr_state": prState, "reconciled": true, "swept": true,
		})
		log.Printf("pr-reconcile sweep: task %s → %s (pr_state=%s)", t.ID, target, prState)
	}
	if converged > 0 {
		log.Printf("pr-reconcile sweep: converged %d stranded PR task(s)", converged)
	}
}

// sweepExpiredLeases (G4) requeues tasks held by a dead agent past lease expiry
// and escalates each recovery: an event for SSE/notify plus a nudge to the
// profile so a live agent re-claims the now-pending task.
func (r *Relay) sweepExpiredLeases() {
	swept, err := r.DB.SweepExpiredLeases()
	if err != nil {
		log.Printf("lease sweep: %v", err)
		return
	}
	for _, s := range swept {
		r.Events.Emit(MCPEvent{
			Type: "task", Action: "lease_reclaimed", Agent: "relay-sweeper",
			Project: s.Project, Target: s.Profile, Label: s.Title, Priority: s.Priority,
		})
		// Nudge the profile so the requeued task is picked up, not just logged.
		if r.Registry != nil && s.Profile != "" {
			r.Registry.NotifyProfile(s.Project, s.Profile, "relay-sweeper",
				"Task requeued (lease of dead holder "+s.From+" expired): "+s.Title, s.TaskID)
		}
		log.Printf("lease sweep: requeued task %s (dead holder %q, lease expired) → pending", s.TaskID, s.From)
	}
	if len(swept) > 0 {
		log.Printf("lease sweep: recovered %d stuck task(s)", len(swept))
	}
}
