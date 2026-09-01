package linear

import (
	"fmt"

	"agent-relay/internal/db"
	"agent-relay/internal/models"
)

// linearRefusedStatus is the mirror status of a Linear issue refused for missing
// its typed ticket. The ruling amends the original "no mirror" stance (V1-bis):
// a refused issue DOES persist a mirror row, but in this dedicated state — never
// dispatched, never surfaced as an assignable/active task (the status-filtered
// queries for pending/accepted/in-progress/blocked all exclude it), and carrying
// the refusal_notified_at anti-spam marker. One row, one anti-spam mechanism,
// shared by the webhook and the poll — instead of two event-dedupe schemes that
// would diverge and break on replays.
const linearRefusedStatus = "refused"

// refusalDecision tells a caller whether to run its normal mirror+dispatch path.
type refusalDecision int

const (
	proceedNormal refusalDecision = iota // conforming (or exempt) — mirror + dispatch normally
	refusedHold                          // non-conforming — refused row persisted; do NOT dispatch
)

// handleTypedTicket enforces typed-ticket discipline for an agent-targeted Linear
// issue on a require-typed-ticket project. It is the ONE place both arrival paths
// (webhook Ingest, reconcile poll) run their refusal logic through, so no issue
// dies silently regardless of how it arrives, and neither path can spam.
//
// Callers invoke it only when the project requires a typed ticket AND the issue's
// resolved target is an agent — a non-agent issue was never dispatchable, so
// refusing it would be noise (and the poll already skips it). `existing` is the
// mirror row as it stands BEFORE this call (the caller's single read); the caller
// reuses it for its own dispatch dedupe, so this method never re-reads it.
//
// Outcomes:
//   - conforming: proceedNormal. If the issue was previously refused, its marker
//     is cleared so a later regression re-notifies once; the caller's upsert then
//     flips the row out of "refused" and it dispatches on its next started state.
//   - non-conforming, refusable row (first sight / already-refused / still-pending
//     unstarted): upserts/keeps a REFUSED row; the primary mirror ALSO posts the
//     loud comment EXACTLY once (marker-deduped across the webhook and every poll
//     cycle) — refusedHold either way.
//   - non-conforming, in-flight/terminal row (accepted/in-progress/in-review/
//     blocked/done/cancelled): proceedNormal WITHOUT refusing — work already in
//     flight is never retro-refused (the reconcile dispatch-gate still blocks any
//     re-dispatch of a non-conforming issue).
//
// primary gates the Linear-facing side effect only (the loud comment + its
// anti-spam marker): a fanned-out issue shares ONE Linear issue across several
// mirror rows, so only the primary (index 0 of projectsFor) may post to it — a
// secondary mirror still gets its own refused, non-dispatching row (silently),
// matching the primary-mirror-only write-back rule used for status pushes.
func (c *Connector) handleTypedTicket(iss gqlIssue, seed db.LinearMirrorSeed, existing *models.Task, primary bool) (refusalDecision, error) {
	missing := parseTicket(iss.Description).missing

	// Conforming — nothing to refuse. Clear a stale marker so a future regression
	// re-notifies once (AC4). The caller's normal upsert flips status out of
	// "refused" back to the Linear-mapped status.
	if len(missing) == 0 {
		if existing != nil && existing.Status == linearRefusedStatus {
			if err := c.db.ClearRefusalNotified(existing.ID); err != nil {
				return proceedNormal, err
			}
		}
		return proceedNormal, nil
	}

	// Non-conforming. Never retro-refuse work already in flight: only a first
	// sighting (existing == nil), an already-refused row, or a still-pending
	// (unstarted) mirror is refusable. Anything the agent has begun or that is
	// terminal is left for the caller's normal (non-dispatching) path.
	if existing != nil && existing.Status != linearRefusedStatus && existing.Status != "pending" {
		return proceedNormal, nil
	}

	// Persist / keep the refused row. UpsertLinearMirror never touches
	// refusal_notified_at, so the marker survives every re-poll of the same
	// non-conforming issue — that is what makes the anti-spam hold.
	seed.Status = linearRefusedStatus
	taskID, _, err := c.db.UpsertLinearMirror(seed)
	if err != nil {
		return refusedHold, fmt.Errorf("upsert refused mirror: %w", err)
	}

	// Only the primary mirror ever touches Linear — a secondary fan-out mirror
	// stops here with its own refused row persisted, no comment.
	if !primary {
		return refusedHold, nil
	}

	// Comment exactly once: only when the marker is unset (first refusal, or a
	// prior refused row whose comment never landed). If the comment fails, leave
	// the marker UNSET and surface the error so the caller re-drives it next cycle
	// — the refused row already blocks dispatch meanwhile. Once the comment lands,
	// stamp the marker best-effort: a stamp hiccup must not re-drive the ingest
	// (that would re-comment), and the next cycle re-attempts the stamp anyway.
	if existing == nil || existing.RefusalNotifiedAt == nil {
		if err := c.Comment(iss.ID, refusalComment(missing)); err != nil {
			return refusedHold, fmt.Errorf("typed-ticket refusal comment: %w", err)
		}
		_ = c.db.SetRefusalNotified(taskID)
	}
	return refusedHold, nil
}
