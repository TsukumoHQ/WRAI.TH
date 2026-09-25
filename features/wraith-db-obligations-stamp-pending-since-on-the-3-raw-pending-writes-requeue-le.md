# [wraith/db][obligations] stamp pending_since on the 3 raw ->pending writes (requeue, lease expiry, orphan cascade)

## Team : wraith-engine (tsukumo)
## Branch : wraith/engine-58ece5e2 (from main)
## Relay task : 58ece5e2-0156-4edc-b7c4-904948557eae
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 requeue stamps: task with pending_since 30 days ago, accepted, then RequeueTask; afterwards pending_since equals the requeue timestamp (within the same second as last_activity_at) and an ACK sweep at requeue+1min opens nothing beyond rung 0. Test TestPendingSinceStampedOnRequeue.
- [ ] 2. AC2 lease expiry stamps: in-progress task whose lease is expired is released to pending by the lease sweep; pending_since equals the release time, not the prior value. Test TestPendingSinceStampedOnLeaseExpiry.
- [ ] 3. AC3 cascade stamps: task assigned to an agent that is removed is cascaded back to pending by referential_cascade; pending_since equals the cascade time. Test TestPendingSinceStampedOnOrphanCascade.

## 2. Root cause & decisions

ROOT_CAUSE: c5288ca restarts the ACK clock (tasks.pending_since) only in the dispatch INSERT and in transitionTask's pending branch. Three raw UPDATEs also move a task back to 'pending' without going through transitionTask: RequeueTask (internal/db/watchdog.go:148), the expired-lease release in SweepExpiredLeases (internal/db/task_lease.go:245) and the leased-task release in CascadeAgentDeactivation (internal/db/referential_cascade.go:77). A task released by any of them kept its old pending_since, so if it had been claimed before its obligations were instantiated, the first sweep after the release saw an age of hours or weeks and could fire the top rung.
DECISION: add pending_since = ? (the same now each UPDATE already writes to last_activity_at) to the three UPDATEs. No schema change, no handler change, dispatched_at untouched, CAS guards unchanged.
REJECTED: routing the three paths through transitionTask (it clears more columns and emits a different audit trail; the raw UPDATEs are CAS-guarded on lease_holder, which transitionTask is not).

DoD grep (`grep -n "status *= *'pending'" internal/db/*.go`, non-test): the only UPDATEs that SET status to 'pending' are watchdog.go:148, task_lease.go:245 and referential_cascade.go:77, and each now sets pending_since:
internal/db/watchdog.go:151            last_activity_at = ?, pending_since = ?
internal/db/task_lease.go:246          lease_expires_at=NULL, lease_heartbeat_at=NULL, last_activity_at=?, pending_since=?
internal/db/referential_cascade.go:78  lease_expires_at=NULL, lease_heartbeat_at=NULL, last_activity_at=?, pending_since=?
The other hits are WHERE/SELECT predicates or UPDATEs of other columns guarded on status='pending'. The parameterised transitionTask UPDATE into pending (tasks.go:670) already stamps it since c5288ca.

Red on main c5288ca (fix files stashed):
--- FAIL: TestPendingSinceStampedOnRequeue: pending_since = "2026-08-26T20:43:18.194988Z" (old "2026-08-26T20:43:18.194988Z"), want the release time "2026-09-25T20:43:18.195016Z"
--- FAIL: TestPendingSinceStampedOnLeaseExpiry: pending_since = old, want the release time
--- FAIL: TestPendingSinceStampedOnOrphanCascade: pending_since = old, want the release time
Green on branch: all 3 PASS.

## review-wraith verdict: SHIP
Scope: internal/db/watchdog.go, internal/db/task_lease.go, internal/db/referential_cascade.go (one extra column in each CAS UPDATE), internal/db/pending_since_test.go (new, 3 tests).
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -tags fts5 -race ./... OK (all packages).
Checks: single writer kept (the same writerExec, one more bound value); CAS guards unchanged; no schema change; no column-list change.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
internal/db/pending_since_test.go  | 102 +++++++++++++++++++++++++++++++++++++
 internal/db/referential_cascade.go |   4 +-
 internal/db/task_lease.go          |   4 +-
 internal/db/watchdog.go            |   4 +-
 4 files changed, 108 insertions(+), 6 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `58ece5e2-0156-4edc-b7c4-904948557eae`._
