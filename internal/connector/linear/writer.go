package linear

import (
	"context"
	"fmt"
	"time"
)

const (
	writerMaxRetries = 3
	writerRetryBase  = 300 * time.Millisecond
	writerTimeout    = 20 * time.Second
)

// PushInReview is kept for back-compat — a write-back to the team's In Review
// state plus a comment. New callers use PushStatus.
func (c *Connector) PushInReview(linearIssueID, comment string) error {
	return c.PushStatus(linearIssueID, "in-review", comment)
}

// PushStatus moves a Linear issue to the workflow state matching a relay status,
// then posts an optional comment. The two writes an agent/orchestrator may make
// back to Linear — Linear stays the source of truth, the relay only mirrors.
//
// State resolution maps the relay status to a team workflow state by TYPE
// (started/completed/canceled/unstarted) plus NAME for the states Linear models
// by name only ("In Review", "Blocked"). When no state matches (e.g. a team
// without a Blocked state), the move is skipped and the action is recorded as a
// comment instead — honest, never a wrong status. Bounded retries; outcomes go
// to linear_sync_log + the failure counter (surfaced via /api/health).
func (c *Connector) PushStatus(linearIssueID, status, comment string) error {
	if linearIssueID == "" {
		return fmt.Errorf("empty linear issue id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), writerTimeout)
	defer cancel()

	stateID := ""
	if status == "in-review" {
		// Fast path + honors a directly-cached review state.
		if id, err := c.ensureReviewState(ctx); err == nil {
			stateID = id
		}
	}
	if stateID == "" {
		if states, err := c.statesList(ctx); err == nil {
			stateID = resolveStateID(status, states)
		}
	}

	if stateID != "" {
		if err := c.retry(ctx, func() error { return c.gql.issueUpdateState(ctx, linearIssueID, stateID) }); err != nil {
			c.writerFailures.Add(1)
			c.db.LogLinearSync(linearIssueID, status, "error", "issueUpdate: "+err.Error())
			return fmt.Errorf("issueUpdate %s: %w", status, err)
		}
		c.db.LogLinearSync(linearIssueID, status, "ok", "")
	} else {
		// No mappable Linear state — fall back to a comment so the status change
		// is at least recorded, without lying about the issue's state.
		c.db.LogLinearSync(linearIssueID, status, "skip", "no matching Linear state")
		if comment == "" {
			comment = fmt.Sprintf("relay: status → %s (no matching Linear state)", status)
		}
	}

	if comment != "" {
		if err := c.retry(ctx, func() error { return c.gql.commentCreate(ctx, linearIssueID, comment) }); err != nil {
			c.writerFailures.Add(1)
			c.db.LogLinearSync(linearIssueID, "comment", "error", err.Error())
			return fmt.Errorf("commentCreate: %w", err)
		}
		c.db.LogLinearSync(linearIssueID, "comment", "ok", "")
	}
	return nil
}

// Comment posts a standalone comment to a Linear issue (no state change).
func (c *Connector) Comment(linearIssueID, body string) error {
	if linearIssueID == "" {
		return fmt.Errorf("empty linear issue id")
	}
	if body == "" {
		return fmt.Errorf("empty comment body")
	}
	ctx, cancel := context.WithTimeout(context.Background(), writerTimeout)
	defer cancel()
	if err := c.retry(ctx, func() error { return c.gql.commentCreate(ctx, linearIssueID, body) }); err != nil {
		c.writerFailures.Add(1)
		c.db.LogLinearSync(linearIssueID, "comment", "error", err.Error())
		return fmt.Errorf("commentCreate: %w", err)
	}
	c.db.LogLinearSync(linearIssueID, "comment", "ok", "")
	return nil
}

// resolveStateID maps a relay status to a Linear workflow state id, given the
// team's states in workflow order. Returns "" when nothing matches.
func resolveStateID(status string, states []stateInfo) string {
	pick := func(match func(stateInfo) bool) string {
		for _, s := range states {
			if match(s) {
				return s.ID
			}
		}
		return ""
	}
	switch status {
	case "in-review":
		if id := pick(func(s stateInfo) bool { return s.Type == "started" && looksLikeReview(s.Name) }); id != "" {
			return id
		}
		if id := pick(func(s stateInfo) bool { return looksLikeReview(s.Name) }); id != "" {
			return id
		}
		return pick(func(s stateInfo) bool { return s.Type == "started" })
	case "blocked":
		// Linear models "blocked" only by name; "" if the team has no such state.
		return pick(func(s stateInfo) bool { return looksLikeBlocked(s.Name) })
	case "accepted", "in-progress":
		// Prefer a plain started state, not the review/blocked-named one.
		if id := pick(func(s stateInfo) bool {
			return s.Type == "started" && !looksLikeReview(s.Name) && !looksLikeBlocked(s.Name)
		}); id != "" {
			return id
		}
		return pick(func(s stateInfo) bool { return s.Type == "started" })
	case "done":
		return pick(func(s stateInfo) bool { return s.Type == "completed" })
	case "cancelled":
		return pick(func(s stateInfo) bool { return s.Type == "canceled" })
	case "pending":
		for _, t := range []string{"unstarted", "backlog", "triage"} {
			if id := pick(func(s stateInfo) bool { return s.Type == t }); id != "" {
				return id
			}
		}
	}
	return ""
}

// retry runs fn with bounded exponential backoff, honoring ctx cancellation.
func (c *Connector) retry(ctx context.Context, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < writerMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(writerRetryBase * time.Duration(1<<uint(attempt-1))):
			}
		}
		if lastErr = fn(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}
