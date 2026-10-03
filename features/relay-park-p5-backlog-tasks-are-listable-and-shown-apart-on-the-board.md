# [relay/park P5] backlog tasks are listable and shown apart on the board

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/e6ee47b4-backlog-list (from main)
## Relay task : e6ee47b4-07b3-4ede-bb0e-680d4604c904
## Trace : trace=9e09641701f2c1381691b485ddd607d5
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: list_tasks status='backlog' returns only backlog tasks
- [ ] 2. test: list_tasks ready=true never returns a backlog task
- [ ] 3. board v2 renders backlog tasks in their own column (screenshot or DOM test in PR body)
- [ ] 4. skill/relay.md has one paragraph naming when to use backlog:true, pending, park_task, block_task
- [ ] 5. go test -tags fts5 ./... green

## 2. Root cause & decisions

ROOT_CAUSE: the DB already filters list_tasks by any status, but the list_tasks schema enum offered no 'backlog', so agents could not ask for groomed work on its own. On the v2 board, columnFor() had no case for a native 'backlog' task: it fell to the default 'todo' and sat beside claimable work (the Backlog column only caught Linear mirrors whose linear_state matched /backlog/). A backlog dispatch event (action 'backlog') mapped to no status, so a new backlog card never appeared live.

DECISION:
- internal/relay/tools.go: list_tasks status enum gains 'backlog'. Handler and DB unchanged; status='active' semantics unchanged (still all non-done/cancelled, backlog included). Description left as-is. TestToolSchemaBudget after rebase onto origin/main 884413c: `86 tools, 57120 bytes (~14280 tokens), margin 3296 bytes` (go test -tags fts5 ./internal/relay -run TestToolSchemaBudget -v -count=1). Cap not raised; no other tool description touched.
- internal/web/static/v2/api.js: columnFor 'backlog' -> Backlog column; EVENT_STATUS maps 'task.backlog' / action 'backlog' -> 'backlog'.
- internal/web/static/v2/board.js: a backlog event refetches like a dispatch; dropTo ignores a drag of a native backlog card, which would otherwise call REST transition 'pending' (ResetTask) and make it claimable without the promote_task announcement.
- skill/relay.md: one paragraph: backlog:true vs pending vs park_task vs block_task.
- ready=true already excludes backlog (readyTasks is pending-only); test pins it.

AC map:
- C1: TestListTasksStatusBacklogOnlyBacklog (status=backlog returns only the backlog task; schema enum includes 'backlog').
- C2: TestListTasksReadyNeverBacklog (ready=true, with and without status=backlog).
- C3: headless-Chrome DOM test + screenshots, receipt .niwa/receipts/p5-board-backlog-column.txt (script .niwa/receipts/p5-board-shot.mjs, images p5-board-before.png / p5-board-after.png), isolated relay, 2 backlog + 2 pending tasks:
    BEFORE (base binary): todo 4 [2 Ready + 2 Groomed], no Backlog column
    AFTER (this branch):  backlog 2 [Groomed: retire legacy v1 board, Groomed: archive sweep cron]; todo 2 [Ready: list_tasks status=backlog, Ready: board backlog column]
  plus node check of columnFor/eventStatus: {"status":"backlog"} todo before, backlog after; event task/backlog null before, backlog after.
- C4: skill/relay.md "Holding work back": one paragraph (no bullets) under ### Tasks naming backlog:true, pending, park_task, block_task.
- C5: niwa slot run -- go test -tags fts5 ./... exit 0 after rebase onto origin/main 884413c (internal/db 75.7s, internal/relay 91.3s, all ok).

RED_EVIDENCE:
  cmd: niwa slot run -- go test -tags fts5 ./internal/relay -run 'TestListTasks(StatusBacklogOnlyBacklog|ReadyNeverBacklog)'
  test_sha: c7b748f
  output: |
    --- FAIL: TestListTasksStatusBacklogOnlyBacklog (0.06s)
        list_backlog_test.go:64: list_tasks status enum [pending accepted in-progress done blocked cancelled active] lacks 'backlog'
    FAIL	agent-relay/internal/relay	0.575s

## review-wraith verdict: SHIP
Scope: internal/relay/tools.go, internal/relay/list_backlog_test.go, internal/web/static/v2/api.js, internal/web/static/v2/board.js, skill/relay.md.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (via niwa slot run); node --check on both JS files OK.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- dropping a pending card onto the Backlog column still writes 'pending' (pre-existing COLUMN_STATUS.backlog), i.e. no demote by drag; demote stays a tool call with its authority checks.
Notes: no schema change, no DB write path touched, no new tool (schema +9 bytes).

## 3. Files changed

```
internal/relay/list_backlog_test.go | 84 +++++++++++++++++++++++++++++++++++++
 internal/relay/tools.go             |  2 +-
 internal/web/static/v2/api.js       |  2 +
 internal/web/static/v2/board.js     |  5 ++-
 skill/relay.md                      |  6 +++
 5 files changed, 97 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `e6ee47b4-07b3-4ede-bb0e-680d4604c904`._
