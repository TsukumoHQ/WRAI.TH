package relay

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"agent-relay/internal/db"
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
				r.sweepReferentialIntegrity()
				r.sweepLimboAssignees()
				r.sweepDanglingBoards()
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

// sweepReferentialIntegrity (Phase 0/2 GC) re-runs the referential scan so
// orphans that appeared during uptime — most importantly a task whose assignee
// was deactivated since the last sweep — are re-detected and quarantined, and
// refs that healed (the missing agent registered) are stamped resolved. This is
// the PERIODIC re-scan deferred from Phase 0; it rides this existing 2-minute
// ticker rather than adding a goroutine or a heavier hot-path write. Detection
// only — it changes no task/agent behavior. Idempotent, so a clean sweep is a
// cheap near-no-op.
func (r *Relay) sweepReferentialIntegrity() {
	counts, err := r.DB.RunReferentialScan()
	if err != nil {
		log.Printf("integrity sweep: %v", err)
		return
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total > 0 {
		log.Printf("integrity sweep: %d open quarantine row(s) across %d class(es)", total, len(counts))
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

// limboSweepApply decides whether the limbo sweep WRITES or only dry-runs.
// Dry-run is the DEFAULT (safe first deploy): the sweep computes and journals
// its dispositions but changes nothing. Writes are enabled only by an explicit
// opt-in — the env RELAY_LIMBO_SWEEP_APPLY (1/true/yes) or, absent an env
// override, the DB setting limbo_sweep_apply='1'. The env wins so an operator
// can force dry-run or apply at boot without a DB write; a DB setting lets it be
// flipped live once the first real runs have been audited.
func (r *Relay) limboSweepApply() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_LIMBO_SWEEP_APPLY"))) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return r.DB.GetSetting("limbo_sweep_apply") == "1"
}

// sweepLimboAssignees (A1-ii) blocks (>7d) then archives (>30d) tasks stranded
// on an inactive assignee, so no task stays claimed for life by a dead agent.
// Every disposition is journaled with the `integrity:` prefix (consistent with
// the referential/A2 logging). In apply mode it also sends ONE digest per
// dispatcher of that sweep's newly-blocked tasks — but only to a dispatcher that
// is itself still active; for a gone/inactive dispatcher the digest is a journal
// line only, never a message into the void.
func (r *Relay) sweepLimboAssignees() {
	apply := r.limboSweepApply()
	res, err := r.DB.SweepLimboAssignees(time.Now(), apply)
	if err != nil {
		log.Printf("integrity: limbo sweep error: %v", err)
		return
	}
	mode := "apply"
	if res.DryRun {
		mode = "dry-run"
	}
	for _, b := range res.Blocked {
		log.Printf("integrity: limbo tier=1 action=block mode=%s task=%s project=%s assignee=%q from=%s reason=%q",
			mode, b.TaskID, b.Project, b.AssignedTo, b.FromStatus, b.Reason)
	}
	for _, a := range res.Archived {
		log.Printf("integrity: limbo tier=2 action=archive mode=%s task=%s project=%s assignee=%q dispatcher=%q",
			mode, a.TaskID, a.Project, a.AssignedTo, a.DispatchedBy)
	}
	if len(res.Blocked)+len(res.Archived) > 0 {
		log.Printf("integrity: limbo sweep (%s) — %d blocked, %d archived (scanned %d)",
			mode, len(res.Blocked), len(res.Archived), res.Scanned)
	}
	if !res.DryRun {
		r.digestLimboBlocks(res.Blocked)
	}
}

// danglingBoardApply decides whether the dangling-board sweep WRITES or only
// dry-runs, mirroring limboSweepApply exactly. Dry-run is the DEFAULT (safe
// first deploy). Writes are enabled only by an explicit opt-in — the env
// RELAY_DANGLING_BOARD_APPLY (1/true/yes) or, absent an env override, the DB
// setting dangling_board_apply='1'. The env wins so an operator can force
// dry-run or apply at boot without a DB write; a DB setting lets it be flipped
// live once the first real runs have been audited.
func (r *Relay) danglingBoardApply() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_DANGLING_BOARD_APPLY"))) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return r.DB.GetSetting("dangling_board_apply") == "1"
}

// sweepDanglingBoards re-homes native tasks whose board_id points at a missing
// or archived board onto their profile's product board (Q4 residual). Every
// disposition is journaled with the `integrity:` prefix; no digest messages are
// sent (unlike the limbo sweep) — a re-home is not something a dispatcher needs
// pinged about. Dry-run by default; idempotent (a re-run after an apply pass
// finds no candidates and journals only the summary, if anything).
func (r *Relay) sweepDanglingBoards() {
	res, err := r.DB.SweepDanglingBoards(r.danglingBoardApply())
	if err != nil {
		log.Printf("integrity: dangling-board sweep error: %v", err)
		return
	}
	mode := "apply"
	if res.DryRun {
		mode = "dry-run"
	}
	for _, d := range res.Dispositions {
		log.Printf("integrity: dangling-board action=%s mode=%s task=%s project=%s from=%s to=%s slug=%s",
			d.Action, mode, d.Task, d.Project, d.FromBoard, d.ToBoard, d.Slug)
	}
	if len(res.Dispositions) > 0 {
		log.Printf("integrity: dangling-board sweep (%s) — %d disposition(s) (scanned %d)",
			mode, len(res.Dispositions), res.Scanned)
	}
}

// digestLimboBlocks sends one grouped digest per dispatcher of the tasks blocked
// this sweep. A dispatcher that is gone or inactive gets a journal line instead
// of a message (nobody live to read it). Never fired in dry-run.
func (r *Relay) digestLimboBlocks(blocked []db.LimboDisposition) {
	if r.Registry == nil || len(blocked) == 0 {
		return
	}
	// group task ids by (project, dispatcher); preserve first-seen order.
	type key struct{ project, dispatcher string }
	order := []key{}
	byDispatcher := map[key][]string{}
	for _, b := range blocked {
		if b.DispatchedBy == "" {
			continue
		}
		k := key{b.Project, b.DispatchedBy}
		if _, ok := byDispatcher[k]; !ok {
			order = append(order, k)
		}
		byDispatcher[k] = append(byDispatcher[k], b.TaskID)
	}
	for _, k := range order {
		ids := byDispatcher[k]
		subject := fmt.Sprintf("%d task(s) blocked — assignee inactive: %s", len(ids), strings.Join(ids, ", "))
		if r.DB.AgentActive(k.project, k.dispatcher) {
			r.Registry.Notify(k.project, k.dispatcher, "relay-sweeper", subject, "")
		} else {
			log.Printf("integrity: limbo digest suppressed (dispatcher %q inactive) project=%s: %s",
				k.dispatcher, k.project, subject)
		}
	}
}
