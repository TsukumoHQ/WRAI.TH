# [relay/events] cancel_task emits the task.cancelled semantic event the v2 board listens for

## Team : wraith-engine (tsukumo)
## Branch : wraith-engine/14fe2cf5-cancel-event (from main)
## Relay task : 14fe2cf5-3fdb-468d-a863-903e9692c67c
## Trace : trace=7b2560e06a61163579c58e1685d93c99
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. cancel_task on a pending task publishes exactly one semantic event with Type task.cancelled and Semantic task_id equal to the task id (test)
- [ ] 2. when a reason is given the event carries it under Semantic reason (test)
- [ ] 3. a cancel_task that fails (unknown task id) publishes no task.cancelled event (test)
- [ ] 4. go test -tags fts5 ./internal/relay/ green; go vet -tags fts5 ./... clean

## 2. Root cause & decisions

ROOT_CAUSE: HandleCancelTask (internal/relay/handlers_tasks.go) never called emitTaskEvent after a successful CancelTask, unlike HandleBlockTask which emits task.blocked; the v2 board (internal/web/static/v2/board.js:408) listens for type 'task.cancelled' and only reacted through the audit path's action 'cancel', so an MCP cancel never produced the semantic event.
DECISION: emit exactly one task.cancelled (action "cancel") via emitTaskEvent right after a successful CancelTask, carrying standard fields + reason when given; failed cancel returns before the emit. Mirrors the block pattern. No events.go constant, no notification rule, no board.js or REST (api.go) change — REST path rides reason_code slice 2.
BOARD CHECK: board.js:408 removal is guarded by byId.has(id); a second cancel-ish event for an already-removed card falls through, eventStatus() maps neither 'task.cancelled' nor 'cancel', and the else-branch only fires on pending/dispatch -> no-op. No double-apply.
REJECTED: adding task.cancelled to EVENT_STATUS / a notification rule — out of ticket scope.

## review-wraith verdict: SHIP
Scope: internal/relay/handlers_tasks.go (+5, HandleCancelTask emit), internal/relay/handlers_tasks_test.go (+65, TestCancelTaskEmitsCancelledEvent); board.js read-only check
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (1326 passed, 12 pkgs)

BLOCKERS (must fix before merge):
- none (no DB/schema/writer change; emit is in-memory bus after the guarded CancelTask write; failed cancel returns before emit)

NITS (non-blocking):
- none

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/ -run TestCancelTaskEmitsCancelledEvent -count=1
  test_sha: 087dea7
  output: |
    0 matches for '^ok$'
  note: FailedCancelEmitsNothing passes pre-fix by design (regression guard for the no-emit-on-failure AC).

## 3. Files changed

```
internal/relay/handlers_tasks.go      |  5 +++
 internal/relay/handlers_tasks_test.go | 65 +++++++++++++++++++++++++++++++++++
 2 files changed, 70 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `14fe2cf5-3fdb-468d-a863-903e9692c67c`._
