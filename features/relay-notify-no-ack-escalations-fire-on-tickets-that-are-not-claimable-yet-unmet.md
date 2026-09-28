# [relay/notify] no-ACK escalations fire on tickets that are not claimable yet (unmet blocked_by) or are waiting in a pull pool: noise that hides real stalls

## Team : wraith-engine (tsukumo)
## Branch : wraith-engine/887351ac-ack-ready (from main)
## Relay task : 887351ac-2948-4835-9d80-3e39644aa196
## Trace : trace=707236c42f60aafd52fbfbb737cc7039
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. no no-ACK alert for a task with an unmet blocked_by (test)
- [ ] 2. pool task: alert only when ready and the profile's doers have been idle N min (test)
- [ ] 3. stale-accepted alert: accepted/in-progress with no heartbeat or activity for N min alerts the dispatcher once (test)
- [ ] 4. go test -tags fts5 ./... green

## 2. Root cause & decisions

# 887351ac — no-ACK alerts only for READY tasks and idle pools; stale-held alert

ROOT_CAUSE: the ACK ladder skips only tasks with an open task_holds row. 19a1986c / 2c901373 got their blocked_by edges while in backlog; holdIfNotReadyTx holds only a PENDING task and promote to pending opens no hold, so they read as claimable and fired no-ACK. Pool tickets had no notion of a busy doer, and a held (accepted / in-progress) task that went silent had no alert at all.

## Decision
- ackCandidate: READY required (readyPredicate's rule in SQL, hold row or not); pool task not a candidate while a same-profile task is accepted / in-progress.
- ackClock: MAX(pending_since, prerequisite satisfied-at, pool doers' last claim/finish) — timer starts at readiness / pool idleness.
- Stale-held: accepted / in-progress silent for stale_task_age (default 2h) → one P1 STALE notice to the dispatcher per episode (tasks.stale_notified_at CAS, re-armed by activity). in-review excluded.
- Out of scope, flagged: backlog-promote still opens no hold (dispatch claim-signal side).

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db (obligations.go, stale_held.go, db.go), internal/relay (stale_held.go, cleanup.go, settings_spec.go), web v2 settings label, tests
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (8 packages)
BLOCKERS: none
NITS:
- ACK SQL adds correlated subqueries per pending candidate (indexed: idx_org_edges_src, idx_tasks_profile) on the 5-min ticker; no hot-path write.
- Stale sweep writes only on an alert (CAS), additive column out of taskColumns.
- TestACKEquivalence/SeveralTasksOneTick fixture: accepted "d" moved to another profile (same-profile accepted now holds pool tasks back by design).

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/ -run 'TestAckNoAlertWhileBlockedByUnmet|TestAckPoolWaitsWhileProfileDoerBusy' -count=1
  test_sha: 6874693
  output: |
    (run on origin/main 94cd5ed with the AC1/AC2 tests; AC3's evaluateStaleHeldTasks does not exist there)
    --- FAIL: TestAckNoAlertWhileBlockedByUnmet (0.07s)
        zz_red_test.go:38: unmet blocked_by: want no ACK alert, got [{to:user typ:notification priority:P1 action:do subject:Task 'title t1' still no ACK after 120min; dispatcher cto has not re-dispatched it.}]
    --- FAIL: TestAckPoolWaitsWhileProfileDoerBusy (0.08s)
        zz_red_test.go:67: doer busy: want no pool alert, got [{to:user typ:notification priority:P1 action:do subject:Task 'title t1' still no ACK after 120min; dispatcher cto has not re-dispatched it.}]
    FAIL
    FAIL	agent-relay/internal/relay	0.573s

## 3. Files changed

```
internal/db/db.go                              |   6 ++
 internal/db/obligations.go                     |  44 ++++++--
 internal/db/stale_held.go                      |  65 +++++++++++
 internal/relay/cleanup.go                      |   1 +
 internal/relay/obligations_equivalence_test.go |   3 +
 internal/relay/obligations_ready_test.go       | 142 +++++++++++++++++++++++++
 internal/relay/settings_spec.go                |   1 +
 internal/relay/settings_spec_test.go           |   7 +-
 internal/relay/stale_held.go                   |  55 ++++++++++
 internal/web/static/v2/settings.js             |   1 +
 10 files changed, 314 insertions(+), 11 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `887351ac-2948-4835-9d80-3e39644aa196`._
