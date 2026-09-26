# [relay/graph] S1: org_edges + semantics registry + readiness predicate + holds (db)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/graph-s1 (from main)
## Relay task : 019592f0-ec27-4e8c-8e6a-4fcd8b758621
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 registry + guards: TestOrgEdges/UnregisteredTypeRejected (INVALID_ARGUMENT, 0 rows; also disallowed kind pair); /CycleRejected (EDGE_CYCLE; chain over bound -> EDGE_CHECK_LIMIT); /LinearSrcBlockingRefused (LINEAR_READ_ONLY; native blocked_by linear accepted); /TaskColumnsUntouched; /OldDBMigrates.
- [ ] 2. AC2 readiness + holds: /ReadyPredicateSharedByListAndClaim (12 tasks, 10 edges: ListTasks(ready=true) ids == ids ClaimNextTask drains); /HeldUntilReadyExactlyOnce; /InReviewSatisfiesUntilInReview; /PrerequisiteCancelledFlagsNotReleases (held, flagged=prerequisite_cancelled; removing the edge releases); /SettleBounded (100 dependents: <=64 per transition, rest by ReleaseReadyHolds, each once).
- [ ] 3. AC3 claim + discovery: /ClaimNextWalksPastRace (2 concurrent ClaimNextTask on 2 ready tasks -> 2 distinct, never double, never false null); /DiscoveredFromInherits (board, trace, profile inherited when omitted, not when passed; no parent set; discovered_by + origin_holder stamped).

## 2. Root cause & decisions

ROOT_CAUSE: dependencies between tasks existed only in prose ("START after X is submitted": 135 native tasks in 30 d), so only the dispatcher enforced ordering, by hand. The first attempt (depends_on, d5e4ff7) was removed a day later (ade0c39) because it broke Linear mirroring.

DECISION (ruling b3a6ab43, design d523e74e):
- Readiness is derived by one one-hop predicate, never persisted on tasks, so taskColumns is untouched.
- The only state settled at write time is the hold. It is released exactly once by CAS, in the writer tx of the change that made the task ready (<= 64 direct dependents), with ReleaseReadyHolds as the repair sweep.
- Registry-or-reject for edge types. A cancelled prerequisite keeps the dependent held and flagged (prerequisite_cancelled), never released and never auto-cancelled. A blocking edge out of a Linear-mirrored task is refused; native blocked_by linear is allowed.
- until=in-review is set per edge. parent_task_id is untouched. No backfill.

FOUND AND FIXED WHILE TESTING:
1. SQL three-valued logic. With metadata.until absent, `NOT (done OR (in-review AND json_extract(...) = 'in-review'))` evaluates to NULL, and a NULL row drops out of the NOT EXISTS, so the task counted as ready. Every until read is now COALESCE(..., 'done'). /InReviewSatisfiesUntilInReview and /ReadyPredicateSharedByListAndClaim pin this.
2. The bounded cycle walk made chains longer than 256 un-extendable, because every fresh dispatch walked the whole chain. A task that nothing depends on cannot close a cycle, so the walk is now skipped when src has no dependents. The bound still refuses closing a long chain (/CycleRejected).

NOTES FOR S2: Task.Released carries the ids to announce after the handler's commit. TaskHeld tells dispatchCore to skip announceClaimable. TaskReadiness returns the unsatisfied prerequisites the claim warning must name (ruling). A backlog task dispatched with blocked_by gets no hold, and promote_task announces it as today; S2 should decide whether promote must check readiness.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/{db.go,org_edges.go,tasks.go,org_edges_test.go}, internal/models/task.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -count=1 -tags fts5 -race OK (1170 passed, 12 packages; rebased on 66bab0c)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- transitionTask does one indexed RO read (hasBlockingDependentsRO) on moves into or out of in-review/done/cancelled, and a tx only when held dependents exist. A hold created between that read and the CAS is released by the sweep.
- ClaimNextTask(sort=unblock_impact) runs a bounded BFS per ready candidate (fine at current scale; a persisted count is OUT per the design).
- Single writer kept: the dispatch edges, the hold and the settle all ride existing writer txs; reads use d.ro(). models.Task gains transient, non-scanned fields only (json "-"/omitempty), following the LeaseTransfer precedent.

## 3. Files changed

```
internal/db/db.go             |   4 +
 internal/db/org_edges.go      | 677 ++++++++++++++++++++++++++++++++++++++++++
 internal/db/org_edges_test.go | 401 +++++++++++++++++++++++++
 internal/db/tasks.go          | 121 +++++++-
 internal/models/task.go       |  14 +
 5 files changed, 1208 insertions(+), 9 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `019592f0-ec27-4e8c-8e6a-4fcd8b758621`._
