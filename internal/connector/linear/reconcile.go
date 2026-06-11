package linear

import (
	"context"
	"log"
	"math/rand"
	"time"
)

const reconcileTimeout = 30 * time.Second

// ReconcileCycle pulls the team's active cycle and upserts every issue into the
// mirror, healing missed webhooks. It is idempotent and never touches the relay
// overlay. The project argument is advisory; rows are stored under the
// connector's configured project so they stay consistent with webhook upserts.
func (c *Connector) ReconcileCycle(_ string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()

	issues, err := c.gql.activeCycleIssues(ctx, c.teamKey)
	if err != nil {
		return 0, err
	}
	if len(issues) == 0 {
		c.lastReconcileAt.Store(time.Now().UnixMilli())
		return 0, nil
	}

	// Pass 1: upsert every issue (creates rows so parent links can resolve).
	upserted := 0
	hasParent := false
	for _, iss := range issues {
		if iss.ID == "" {
			continue
		}
		seed := c.seedFromIssue(iss)
		if _, _, err := c.db.UpsertLinearMirror(seed); err != nil {
			log.Printf("[linear] reconcile upsert %s: %v", iss.ID, err)
			continue
		}
		upserted++
		if iss.parentLinearID() != "" && seed.ParentTaskID == nil {
			hasParent = true
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
		log.Printf("[linear] reconcile healed %d issue(s) in active cycle", n)
	}
}

// jitter returns a non-negative duration in [0, max).
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}
