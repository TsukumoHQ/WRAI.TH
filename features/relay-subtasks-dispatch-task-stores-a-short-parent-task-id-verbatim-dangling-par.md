# [relay/subtasks] dispatch_task stores a short parent_task_id verbatim (dangling parent) and lands the task on another board

## Team : wraith-backend (tsukumo)
## Branch : wraith/98be27bb-dispatch-parent (from main)
## Relay task : 98be27bb-2665-4582-a719-2e8d3cb3faf9
## Trace : trace=f94962c3b674deb884e7bb1abc062ac7
## Status : 🔵 IN REVIEW

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: dispatch_task with an 8-char parent prefix stores the full parent uuid; unknown prefix -> NOT_FOUND
- [ ] 2. test: subtask defaults to the parent's board_id when none given
- [ ] 3. test: blocked_by at dispatch is persisted and returned
- [ ] 4. one-off migration or report: list existing tasks whose parent_task_id is not a full uuid
- [ ] 5. go test -tags fts5 ./... green

## 2. Root cause & decisions

# 98be27bb dispatch_task: short parent prefix, parent board, blocked_by string

ROOT_CAUSE: (1) internal/relay/handlers_tasks.go HandleDispatchTask passed parent_task_id straight from req.GetString to db.DispatchTask (internal/db/tasks.go), which never resolved or checked it, so an 8-char prefix was stored verbatim (dangling parent; the orphan_parent integrity class would later flag it). (2) DispatchTask's board guard (tasks.go "Single board-resolution guard") only inherited a board from discovered_from, never from the parent, so with >1 board a subtask was product-routed (cto's case: e275d710 instead of the parent's 5726abe3) or refused. (3) blocked_by was read with mcp-go GetStringSlice, which returns nil for a string argument, so blocked_by sent as a JSON string was dropped without error and nothing was echoed back.

FIX: db.DispatchTask (the one choke for MCP, REST, batch) resolves parent_task_id via ResolveTaskID, refuses an unknown parent (TaskError TASK_NOT_FOUND), and when board_id is omitted inherits the parent's board (explicit board_id wins; parent before discovered_from); parent trace reuse folded into the same lookup. dispatch_task pre-resolves and answers NOT_FOUND (CodeNotFound) per AC; REST POST /api/tasks maps TASK_NOT_FOUND to 404 (was 500). New stringListArg accepts a native array, a JSON-array string or a comma-separated string for blocked_by (dispatch_task) and blocked_by/blocked_by_remove (update_task); other types refused INVALID_ARGUMENT.

REPORT (AC4, read-only query on prod relay.db 2026-10-03 against tasks with length(parent_task_id) <> 36):
- f790ab79-b548-4715-b939-896567e4a956 (done) parent '33b65821' -> unique match 33b65821-e74c-4b1d-94c4-4b03c62031ff
- 26888454-a7f4-495f-a06b-0271157b79e2 (done) parent '75b1416e' -> unique match 75b1416e-f19e-4ddb-af11-9a60879027cc
- 6664c8c9 already fixed by hand (a9f7d25d-4947-41e2-919c-40b18523974d).
Both remaining rows are done tasks with a unique prefix match; fixable with update_task(parent_task_id=<full>) by their dispatcher. Chose a report over a data migration: two done rows don't justify a write migration on the fleet DB.

## review-wraith verdict: SHIP
Scope: internal/db/tasks.go (DispatchTask parent resolve/check/board), internal/relay/handlers_tasks.go (parent pre-check, stringListArg, update_task blocked_by), internal/relay/api.go (REST 404), internal/relay/parent_dispatch_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK (HEAD) / test -tags fts5 ./... OK

BLOCKERS (must fix before merge):
- none. Single writer unchanged (one extra reader lookup, only when a parent is given); no schema/migration; no hot-path write; tool schemas unchanged (blocked_by stays an array in the schema; string forms are tolerated input).

NITS (non-blocking):
- behaviour change: a dispatch naming a parent in another project (or a garbage parent) is now refused instead of stored dangling. Full suite green, no caller relied on it.

RED_EVIDENCE:
cmd: go test -tags fts5 -count=1 ./internal/relay/ -run TestDispatchParent
test_sha: 25582fd
note: the daemon re-runs cmd at test_sha^. Tests were committed first (7dcfe2c). test_sha is the fix commit 25582fd (fix + the REST-path test), so test_sha^ = 7dcfe2c is the test-first tree where cmd fails (same structure the gate accepted for eae0243b).
output (at 7dcfe2c = 25582fd^, exit 1):
--- FAIL: TestDispatchParent_ShortPrefixStoresFullID
    parent_dispatch_test.go:69: response parent_task_id = 0x47ac8aa1a510, want full f0786d72-f49e-4f5c-9379-63cac6f9951e
--- FAIL: TestDispatchParent_SubtaskInheritsParentBoard
    parent_dispatch_test.go:95: dispatch child without board: {"code":"INVALID_ARGUMENT",... "board_id is required on project 'p1' (2 boards exist)
--- FAIL: TestDispatchParent_BlockedByPersistedAndReturned
    --- FAIL: .../JSON-array_string: response blocked_by = [], want [4191f110-...]
    --- FAIL: .../plain_string: response blocked_by = [], want [4191f110-...]
    parent_dispatch_test.go:149: wrong-typed blocked_by: want INVALID_ARGUMENT, got isErr=false
FAIL	agent-relay/internal/relay

## 3. Files changed

```
...a-short-parent-task-id-verbatim-dangling-par.md |  75 +++++++++
 internal/db/tasks.go                               |  38 ++++-
 internal/relay/api.go                              |   5 +
 internal/relay/handlers_tasks.go                   |  78 ++++++++-
 internal/relay/parent_dispatch_test.go             | 180 +++++++++++++++++++++
 5 files changed, 367 insertions(+), 9 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-98be27bb-2665-4582-a719-2e8d3cb3faf9
- 🟢 AC1: Both MCP and REST paths resolve short prefixes to full UUIDs and refuse unknown prefixes; behavior pinned by tests that exercise real handlers end-to-end and check the stored row, not just call sequences. — evidence: internal/db/tasks.go:311-339 resolves parentTaskID via ResolveTaskID then SELECT board_id/trace_id; tasks.go:328 returns newTaskError(CodeTaskNotFound) for unknown parent. internal/relay/handlers_tasks.go:117-132 does the same for MCP path. — test: TestDispatchParent_ShortPrefixStoresFullID at internal/relay/parent_dispatch_test.go:65 dispatches with parent.ID first 8 chars and asserts child.ParentTaskID equals parent.ID, stored.ParentTaskID equals parent.ID and unknown prefix deadbeef returns NOT_FOUND; TestDispatchParent_RESTResolvesPrefix at parent_dispatch_test.go:148 covers REST 404 on unknown prefix.
- 🟢 AC2: Behavioral test exercises end-to-end handler against real SQLite, asserts stored/returned board_id; not a mock or call-sequence assertion. — evidence: internal/db/tasks.go:333-337 sets boardID to parent board when omitted. Explicit board_id still wins. Single choke db.DispatchTask so MCP REST batch dispatch all inherit parent board by the same code path. — test: TestDispatchParent_SubtaskInheritsParentBoard at internal/relay/parent_dispatch_test.go:99 creates two boards alpha/beta, dispatches parent on beta, then dispatches child with parent_task_id first 8 chars and no board_id, asserts child.BoardID equals beta; second sub-case verifies explicit board_id alpha still wins over parent board.
- 🟢 AC3: Three input shapes covered plus negative case; behavioral end-to-end through HandleDispatchTask, not a mock. Persisted side verified via db.TaskReadiness. — evidence: internal/relay/handlers_tasks.go adds stringListArg accepting native array, JSON-array string, comma-separated string; refused for any other type. internal/db/tasks.go parseBlockedBy applies the resolved strings. update_task uses stringListArg for both blocked_by and blocked_by_remove. — test: TestDispatchParent_BlockedByPersistedAndReturned at internal/relay/parent_dispatch_test.go:131 sub-tests native_array, JSON-array_string, plain_string; each asserts task.BlockedBy equals pre.ID and TaskReadiness shows one ref; final sub-case passes blocked_by=42 and asserts INVALID_ARGUMENT.
- 🔴 AC4: Doer silently dropped AC4. No migration or report to list dangling parent_task_id values. — evidence: AC4 silent drop — test: NONE
- 🟢 AC5: Whole-workspace go test exits 0. — evidence: Ran rtk proxy go test -tags fts5 ./... from review worktree after fetch of origin/main. All packages pass with exit=0. — test: N/A - deterministic gate.

## 5. Timeline

- round 1 → **reject** (review-98be27bb-2665-4582-a719-2e8d3cb3faf9)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `98be27bb-2665-4582-a719-2e8d3cb3faf9`._
