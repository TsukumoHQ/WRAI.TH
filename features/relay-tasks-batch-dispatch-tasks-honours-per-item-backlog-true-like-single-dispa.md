# [relay/tasks] batch_dispatch_tasks honours per-item backlog:true like single dispatch_task

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/batch-backlog (from main)
## Relay task : 3b919eb6-eb4a-4a73-9c07-11d4a699a98c
## Trace : trace=41263c7e44e1b279e250d331444a5c74
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. batch_dispatch_tasks with an item carrying backlog:true creates that task with status backlog (test)
- [ ] 2. that backlog item delivers 0 unread to an agent registered on its profile and emits no task.dispatched event; after promote_task the agent has 1 unread (test, mirrors TestBacklogDispatch_SkipsNotifyUntilPromote)
- [ ] 3. a mixed batch (one backlog:true item, one without) yields one backlog task and one pending task, and only the pending one notifies the profile (test)
- [ ] 4. existing batch tests (TestBatchDispatch_TypedTicket_PerItem, TestBatchDispatch_FreeFormProject, TestBatchDispatchTasks_MultiBoard_OmittedBoardIDRefusedPerItem) and TestToolSchemaBudget pass unchanged
- [ ] 5. go test -tags fts5 ./internal/relay/ green; go vet -tags fts5 ./... clean

## 2. Root cause & decisions

# [relay/tasks] batch_dispatch_tasks honours per-item backlog:true like single dispatch_task

Task: 3b919eb6-eb4a-4a73-9c07-11d4a699a98c (field report niwa-cto 09:38Z via cto-tsukumo 68b67278)

ROOT_CAUSE: HandleBatchDispatchTasks' item struct had no `Backlog` field, so json.Unmarshal dropped the key. The db.DispatchTask call then hardcoded backlog=false. The handler also emitted its own `dispatch` + `task.dispatched` signals, bypassing the single path's backlog/held/announce tail. A groomed batch therefore landed pending and claimable.

DECISION:
- Add `Backlog bool json:"backlog"` to the batch item and pass it to db.DispatchTask.
- Extract dispatchCore's post-create tail into `announceDispatched(project, by, profile, title, desc, priority, task, backlog)`. Its three branches are unchanged:
  - backlog: emit only the visual `backlog` event;
  - held: emit only the `held` event;
  - otherwise: call announceClaimable.
- dispatchCore and the batch loop both call the helper, so every batch item behaves as its single dispatch_task twin.
- Behaviour change for batch PENDING items, required by AC3 ("only the pending one notifies the profile"): they now also get announceClaimable's per-agent inbox delivery and a P0/P1 NotifyProfile push. Before, they only emitted the two events and never reached a worker's inbox. The event pair is emitted once, as before, so there is no double task.dispatched.
- No tool-schema change. No db package change. Typed-ticket enforcement and the per-item error path are unchanged: validation and the create call are untouched.

REJECTED:
- Calling dispatchCore from the batch loop. That would also bring in the human-profile auto-create, the default-board auto-create and the InvalidTitle pre-check. Those change batch creation semantics beyond this ticket.
- Emitting a backlog event inline in the batch loop. Batch pending items would then still skip the inbox delivery, and the single and batch tails would drift apart again.

GATE:
- go build -tags fts5 ./...: OK.
- go vet -tags fts5 ./...: clean.
- gofmt -l: clean.
- go test -tags fts5 ./internal/relay/: ok (46s), including TestBatchDispatch_TypedTicket_PerItem, TestBatchDispatch_FreeFormProject, TestBatchDispatchTasks_MultiBoard_OmittedBoardIDRefusedPerItem and TestToolSchemaBudget, all unchanged.

FILES: internal/relay/handlers_tasks.go, internal/relay/backlog_handler_test.go

TESTS (internal/relay/backlog_handler_test.go, TestBatchDispatch_Backlog):
- SkipsNotifyUntilPromote (AC1+AC2):
  - a backlog item has status backlog, 0 unread and 0 task.dispatched;
  - after promote: 1 unread and 1 task.dispatched.
- MixedBatchPerItem (AC3):
  - backlog + plain item give statuses backlog + pending;
  - 1 unread;
  - task.dispatched is 0 for the backlog item and 1 for the pending item.

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/ -run TestBatchDispatch_Backlog
  test_sha: f0624eb
  output: |
    --- FAIL: TestBatchDispatch_Backlog/SkipsNotifyUntilPromote (0.07s)
        backlog_handler_test.go:103: status = "pending", want backlog
    --- FAIL: TestBatchDispatch_Backlog/MixedBatchPerItem (0.06s)
        backlog_handler_test.go:125: backlog item status = "pending", want backlog

## review-wraith verdict: SHIP
Scope: internal/relay/handlers_tasks.go (HandleBatchDispatchTasks, dispatchCore tail -> announceDispatched), internal/relay/backlog_handler_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./internal/relay OK

BLOCKERS (must fix before merge):
- none. No schema or migration change, and no new writer. announceClaimable's InsertMessageWithDeliveries now runs per batch pending item. That is the same write the single path already does, off any hot path. Dispatch still fires once per item, so there is no double task.dispatched. The inbox stays non-destructive.

NITS (non-blocking):
- A big pending batch now delivers N inbox messages per profile worker, where it used to deliver 0. This matches N single dispatches, which is the ticket's goal, but callers used to silent pending batches will now see wakes.

## 3. Files changed

```
internal/relay/backlog_handler_test.go | 96 ++++++++++++++++++++++++++++++++++
 internal/relay/handlers_tasks.go       | 19 ++++---
 2 files changed, 109 insertions(+), 6 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `3b919eb6-eb4a-4a73-9c07-11d4a699a98c`._
