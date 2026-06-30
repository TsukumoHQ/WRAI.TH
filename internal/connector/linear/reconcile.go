package linear

import (
	"context"
	"log"
	"math/rand"
	"time"
)

const reconcileTimeout = 30 * time.Second

// ReconcileCycle pulls all OPEN issues of the team and upserts every issue into
// the mirror, healing missed webhooks and (without a public webhook endpoint)
// driving auto-dispatch for ANY open issue — not just the active cycle. It is
// idempotent and never touches the relay overlay. Rows are stored under the
// connector's configured project so they stay consistent with webhook upserts.
func (c *Connector) ReconcileCycle(_ string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()

	issues, err := c.gql.openTeamIssues(ctx, c.teamKey)
	if err != nil {
		return 0, err
	}
	// NOTE: do NOT early-return on an empty open set — an empty poll is exactly
	// the Done-dropout case (every issue moved to Done/canceled), and pass 3 must
	// still run to close their now-orphaned mirrors (TSU-159).

	// Pass 1: upsert every issue (creates rows so parent links can resolve).
	// The poll also detects → In Progress transitions so dispatch works without
	// a public webhook endpoint (localhost-only deployments): the prior mirror
	// status is the dedupe — once the row is in-progress, later polls skip it.
	upserted := 0
	hasParent := false
	seen := make(map[string]bool, len(issues)) // every OPEN issue id this poll saw
	for _, iss := range issues {
		if iss.ID == "" {
			continue
		}
		seen[iss.ID] = true
		// Scope: mirror/dispatch issues whose resolved target is an agent — a
		// configured project route, the issue's delegate (Linear's agent-
		// delegation field, for multi-lead projects), or a direct agent assignee.
		// dispatchTarget folds all three (resolved once, reused for dispatch
		// below). Skips Linear's project-less team defaults (onboarding TSU-1..4)
		// and any issue with no agent target — those were polluting the board.
		target := c.dispatchTarget(iss)
		if !isAgent(target) {
			continue
		}
		prior, _ := c.db.GetTaskByLinearIssueID(c.project, iss.ID)
		seed := c.seedFromIssue(iss)
		taskID, _, err := c.db.UpsertLinearMirror(seed)
		if err != nil {
			log.Printf("[linear] reconcile upsert %s: %v", iss.ID, err)
			continue
		}
		upserted++
		if iss.parentLinearID() != "" && seed.ParentTaskID == nil {
			hasParent = true
		}
		// Done echo parity with the webhook path.
		if iss.State != nil && iss.State.Type == "completed" {
			_ = c.db.MarkLinearDone(taskID)
		}
		// Dispatch on a genuine transition into a working "started" state (the
		// agent target is already confirmed by the scope gate above; first sight
		// in-progress counts: boot-time pickup).
		//
		// NEVER re-dispatch a mirror that's already in-progress (avoid a double
		// claim+start) OR already TERMINAL (done/cancelled). The latter is the
		// phantom-stale resurrection: an agent completes the relay task, but the
		// Linear issue lags in a started state (its PR wasn't auto-closed), so
		// every reconcile poll would otherwise re-fire claim+start on work that's
		// done. The webhook path is safe (it gates on a real state change); the
		// poll has no such signal, so it must not resurrect a terminal task. A
		// genuine reopen arrives via the webhook with an actual state transition.
		if c.onEvent != nil &&
			iss.State != nil && iss.State.Type == "started" && !looksLikeReview(iss.State.Name) &&
			(prior == nil || !isTerminalOrActive(prior.Status)) {
			c.onEvent(c.dispatchEvent(taskID, iss.Title, target, seed))
		}
	}

	// Pass 2: re-resolve parent links that weren't yet mirrored on pass 1.
	if hasParent {
		for _, iss := range issues {
			if iss.ID == "" || iss.parentLinearID() == "" {
				continue
			}
			seed := c.seedFromIssue(iss)
			if seed.ParentTaskID == nil {
				continue
			}
			_, _, _ = c.db.UpsertLinearMirror(seed)
		}
	}

	// Pass 3: Done-dropout sync (TSU-159). openTeamIssues returns OPEN issues
	// only, so when an issue is moved to Done/canceled in Linear it drops out of
	// the poll and its mirror never re-upserts — it stays active forever and
	// fires phantom stale-escalations (and a missed webhook on a localhost relay
	// makes it permanent). Reconcile the active mirrors absent from this poll's
	// open set: fetch their real state and close the ones Linear has finished.
	c.syncDroppedMirrors(ctx, seen)

	c.lastReconcileAt.Store(time.Now().UnixMilli())
	return upserted, nil
}

// syncDroppedMirrors closes relay mirror tasks whose Linear issue is no longer
// in the open set because it completed/canceled (TSU-159). It only acts on an
// explicit completed/canceled state from Linear — an issue the API doesn't
// return (e.g. a transient miss or a hard-deleted issue) is left untouched, so a
// blip never cancels live work. A genuine reopen re-enters the open set and
// re-dispatches via the normal path.
func (c *Connector) syncDroppedMirrors(ctx context.Context, seen map[string]bool) {
	active, err := c.db.ActiveLinearMirrors(c.project)
	if err != nil {
		log.Printf("[linear] reconcile dropout-sync list: %v", err)
		return
	}
	taskByIssue := map[string]string{}
	var missing []string
	for _, m := range active {
		if !seen[m.LinearIssueID] {
			missing = append(missing, m.LinearIssueID)
			taskByIssue[m.LinearIssueID] = m.TaskID
		}
	}
	if len(missing) == 0 {
		return
	}
	states, err := c.gql.issuesByIDs(ctx, missing)
	if err != nil {
		log.Printf("[linear] reconcile dropout-sync fetch: %v", err)
		return
	}
	closed := 0
	for _, iss := range states {
		if iss.State == nil {
			continue
		}
		var st string
		switch iss.State.Type {
		case "completed":
			st = "done"
		case "canceled", "cancelled":
			st = "cancelled"
		default:
			continue // still open (e.g. moved to another non-closed state) — leave it
		}
		taskID := taskByIssue[iss.ID]
		if taskID == "" {
			continue
		}
		if err := c.db.CloseLinearMirror(taskID, st); err != nil {
			log.Printf("[linear] reconcile close mirror %s: %v", iss.ID, err)
			continue
		}
		closed++
	}
	if closed > 0 {
		log.Printf("[linear] reconcile dropout-sync: closed %d mirror(s) for Done/canceled issues", closed)
	}
}

// StartReconcile runs the active-cycle reconcile poll on an interval with a
// small jitter, one in flight at a time (the loop is sequential). It warms the
// caches (viewer id + In Review state) once at startup. Stops when done closes.
func (c *Connector) StartReconcile(interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		// Warm caches and run an initial reconcile shortly after boot (jittered),
		// so a freshly started relay is current without waiting a full interval.
		warmCtx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
		c.Warmup(warmCtx)
		cancel()

		initial := time.NewTimer(jitter(2 * time.Second))
		defer initial.Stop()
		select {
		case <-done:
			return
		case <-initial.C:
			c.runReconcile()
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// Jitter each run to avoid thundering herd against Linear.
				time.Sleep(jitter(interval / 4))
				c.runReconcile()
			}
		}
	}()
}

func (c *Connector) runReconcile() {
	n, err := c.ReconcileCycle(c.project)
	if err != nil {
		log.Printf("[linear] reconcile error: %v", err)
		return
	}
	if n > 0 {
		log.Printf("[linear] reconcile healed %d open team issue(s)", n)
	}
}

// jitter returns a non-negative duration in [0, max).
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}
