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
	if len(issues) == 0 {
		c.lastReconcileAt.Store(time.Now().UnixMilli())
		return 0, nil
	}

	// Pass 1: upsert every issue (creates rows so parent links can resolve).
	// The poll also detects → In Progress transitions so dispatch works without
	// a public webhook endpoint (localhost-only deployments): the prior mirror
	// status is the dedupe — once the row is in-progress, later polls skip it.
	upserted := 0
	hasParent := false
	for _, iss := range issues {
		if iss.ID == "" {
			continue
		}
		// Scope: mirror/dispatch issues that are EITHER in a ROUTED project
		// (owner-chosen agent per project) OR directly assigned to an agent
		// (delegate-based routing — a project with many leads, each issue
		// assigned to its own lead). This must stay symmetric with the dispatch
		// condition below: an issue that would dispatch must not be pre-skipped
		// here, or the poll path (the only path on a webhook-less localhost)
		// silently drops it. Still skips Linear's project-less team defaults
		// (onboarding TSU-1..4) and any unrouted, unassigned issue — those were
		// polluting the relay board as noise.
		if !c.hasRoute(iss) && !isAgent(issueAssignee(iss)) {
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
		// Dispatch on a genuine transition into a working "started" state with
		// an agent assignee (first sight in-progress counts: boot-time pickup).
		if c.onEvent != nil &&
			iss.State != nil && iss.State.Type == "started" && !looksLikeReview(iss.State.Name) &&
			(c.hasRoute(iss) || isAgent(issueAssignee(iss))) &&
			(prior == nil || prior.Status != "in-progress") {
			c.onEvent(c.dispatchEvent(taskID, iss.Title, seed))
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

	c.lastReconcileAt.Store(time.Now().UnixMilli())
	return upserted, nil
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
