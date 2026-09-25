# [wraith/db][obligations] ACK clock keys on tasks.pending_since (promote from backlog resets the clock)

## Team : wraith-engine (tsukumo)
## Branch : wraith/engine-c933b2f1 (from main)
## Relay task : c933b2f1-4bd3-4e91-bb7e-de9f725504ec
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 promote resets clock: task dispatched with dispatched_at 30 days ago, status backlog, then PromoteTask; a sweep at promote+1min opens no obligation beyond rung 0 (no ack.human/ack.manager row, no message to user); a sweep at promote + first-rung deadline opens rung 0 only. Test TestAckClockPromoteFromBacklogResets.
- [ ] 2. AC2 dispatch stamps: a freshly dispatched task has pending_since == dispatched_at (string-equal) and the ACK chain timing is identical to main for it (existing obligations tests pass unchanged). Test TestAckClockDispatchStampsPendingSince.
- [ ] 3. AC3 backfill: DB opened with a pre-migration tasks table containing rows with NULL pending_since gets pending_since == dispatched_at on every row after migration; running the migration twice changes nothing. Test TestPendingSinceBackfillEqualsDispatchedAt.

## 2. Root cause & decisions

ROOT_CAUSE: the ACK clock was tasks.dispatched_at (obligations.go ackCandidate + ActiveTaskObligations/TaskObligationByID/MyTaskObligations, and the legacy GetUnackedTasks). PromoteTask -> transitionTask never touched it, so a task groomed in backlog for weeks and then promoted had an age of weeks the moment it became pending. The first sweep opened the chain and fired the top rung (misfire 27a77033: promoted 15:57:34Z, ack.human to=user 16:00:57Z 'no ACK after 25110min').
DECISION (ruling cto-tsukumo 73a3b32b, option b): additive nullable tasks.pending_since, backfilled = dispatched_at by the same idempotent migration (only NULLs). Dispatch stamps it = dispatched_at. transitionTask stamps it = now on every move INTO pending (promote, unblock, reset). Every ACK clock read and order keys on COALESCE(t.pending_since, t.dispatched_at) (one ackClock constant), so linear-mirror inserts and older binaries keep the old clock. dispatched_at is never rewritten. pending_since stays out of taskColumns/scanTask (like trace_id), so the column-count lockstep is untouched. No handler/API change.
REJECTED: rewriting dispatched_at on promote (destroys dispatch history, and dispatch-ordered queries would reorder); a separate promoted_at column (unblock/reset would need their own columns for the same clock).

EVIDENCE (read-only copy of ~/.agent-relay/_redeploy_bak/relay.db.pre-b559017-20260908T000022Z in the session scratchpad, deleted after; the live DB was never opened):
before: pending_since column absent, 2358 tasks
after New() migration: sqlite3 <copy> "select count(*) from tasks where pending_since is null or pending_since != dispatched_at" = 0 (of 2358)

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/db.go (column + backfill), internal/db/tasks.go (dispatch INSERT, transitionTask pending branch, GetUnackedTasks), internal/db/obligations.go (ackClock at every clock read/order), internal/db/obligations_test.go (3 AC tests), internal/db/ack_scanner_test.go (fixture: backdate pending_since with dispatched_at).
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -tags fts5 -race ./... OK (all packages, including relay TestACKEquivalence and the chain tests). The 3 new tests fail on main and pass here.
Schema: additive + idempotent (ensureColumns + UPDATE ... WHERE pending_since IS NULL). An older binary on a migrated DB ignores the column. A newer binary on rows it didn't write falls back to dispatched_at via COALESCE. No agentColumns/taskColumns change. The single writer is kept: the stamp rides the existing CAS UPDATE.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/ack_scanner_test.go is a 5th file: its fixture backdated only dispatched_at, which is no longer the ACK clock for a dispatched task. It is a fixture-only change.
- Follow-up, out of scope: RequeueTask's raw UPDATE to 'pending' (internal/db/watchdog.go:148) does not stamp pending_since, so a requeued task keeps its earlier clock. That is the same behaviour as main, but it is the same misfire class if the task was never instantiated before its claim.

## 3. Files changed

```
internal/db/ack_scanner_test.go |   5 +-
 internal/db/db.go               |  12 ++++
 internal/db/obligations.go      |  21 +++---
 internal/db/obligations_test.go | 143 ++++++++++++++++++++++++++++++++++++++++
 internal/db/tasks.go            |  14 ++--
 5 files changed, 179 insertions(+), 16 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c933b2f1-4bd3-4e91-bb7e-de9f725504ec`._
