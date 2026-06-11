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

// PushInReview is the agent's single owned write-back: move the issue to the
// team's In Review state, then post a comment. Bounded exponential retries.
// Outcomes are logged to the linear_sync_log audit table and reflected in the
// connector's failure counter (surfaced via /api/health).
func (c *Connector) PushInReview(linearIssueID, comment string) error {
	if linearIssueID == "" {
		return fmt.Errorf("empty linear issue id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), writerTimeout)
	defer cancel()

	stateID, err := c.ensureReviewState(ctx)
	if err != nil {
		c.writerFailures.Add(1)
		c.db.LogLinearSync(linearIssueID, "in_review", "error", "resolve state: "+err.Error())
		return fmt.Errorf("resolve in-review state: %w", err)
	}

	if err := c.retry(ctx, func() error { return c.gql.issueUpdateState(ctx, linearIssueID, stateID) }); err != nil {
		c.writerFailures.Add(1)
		c.db.LogLinearSync(linearIssueID, "in_review", "error", "issueUpdate: "+err.Error())
		return fmt.Errorf("issueUpdate in-review: %w", err)
	}
	c.db.LogLinearSync(linearIssueID, "in_review", "ok", "")

	if comment != "" {
		if err := c.retry(ctx, func() error { return c.gql.commentCreate(ctx, linearIssueID, comment) }); err != nil {
			// The transition is the load-bearing write; a failed comment is logged
			// but not fatal (the state already moved).
			c.writerFailures.Add(1)
			c.db.LogLinearSync(linearIssueID, "comment", "error", err.Error())
			return fmt.Errorf("commentCreate: %w", err)
		}
		c.db.LogLinearSync(linearIssueID, "comment", "ok", "")
	}
	return nil
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
