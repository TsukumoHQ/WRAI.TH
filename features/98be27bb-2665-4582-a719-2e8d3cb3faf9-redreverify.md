# ⚠ untyped record — no title or acceptance criteria synced (task 98be27bb)

## Team : wraith-backend (agent-relay)
## Branch : wraith/98be27bb-dispatch-parent (from main)
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
_(untyped ticket — no acceptance criteria)_

## 2. Root cause & decisions

# 98be27bb dispatch_task: short parent prefix, parent board, blocked_by string

ROOT_CAUSE: (1) internal/relay/handlers_tasks.go HandleDispatchTask passed parent_task_id straight from req.GetString to db.DispatchTask (internal/db/tasks.go), which never resolved or checked it, so an 8-char prefix was stored verbatim (dangling parent; the orphan_parent integrity class would later flag it). (2) DispatchTask's board guard (tasks.go "Single board-resolution guard") only inherited a board from discovered_from, never from the parent, so with >1 board a subtask was product-routed (cto's case: e275d710 instead of the parent's 5726abe3) or refused. (3) blocked_by was read with mcp-go GetStringSlice, which returns nil for a string argument, so blocked_by sent as a JSON string was dropped without error and nothing was echoed back.

FIX: db.DispatchTask (the one choke for MCP, REST, batch) resolves parent_task_id via ResolveTaskID, refuses an unknown parent (TaskError TASK_NOT_FOUND), and when board_id is omitted inherits the parent's board (explicit board_id wins; parent before discovered_from); parent trace reuse folded into the same lookup. dispatch_task pre-resolves and answers NOT_FOUND (CodeNotFound) per AC; REST POST /api/tasks maps TASK_NOT_FOUND to 404 (was 500). New stringListArg accepts a native array, a JSON-array string or a comma-separated string for blocked_by (dispatch_task) and blocked_by/blocked_by_remove (update_task); other types refused INVALID_ARGUMENT.

AC4 (round 1 finding: report lived only in this file): internal/db/short_parent_backfill.go, wired into migrate() after the product board routing backfill (internal/db/db.go). listShortParentRefs reports every task whose parent_task_id is set but not 36 chars, with its same-project prefix matches; backfillShortParents logs each one, rewrites a unique same-project match to the full id, leaves ambiguous/unmatched refs as-is with their match count. Settings marker backfill_short_parent_ids; idempotent (a repaired row is no longer selected). Test: internal/db/short_parent_backfill_test.go (unique repaired; ambiguous, unmatched and cross-project untouched + logged; second run no-op).
Prod today (read-only query): f790ab79 -> 33b65821-e74c-4b1d-94c4-4b03c62031ff, 26888454 -> 75b1416e-f19e-4ddb-af11-9a60879027cc (both done, unique) — the backfill repairs both on next deploy; 6664c8c9 already fixed by hand.

## review-wraith verdict: SHIP
Scope: internal/db/tasks.go (DispatchTask parent resolve/check/board), internal/db/short_parent_backfill.go (+test, migrate hook), internal/relay/handlers_tasks.go (parent pre-check, stringListArg, update_task blocked_by), internal/relay/api.go (REST 404), internal/relay/parent_dispatch_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK (HEAD) / test -tags fts5 ./... OK

BLOCKERS (must fix before merge):
- none. Single writer unchanged (one extra reader lookup, only when a parent is given); no schema change; one marker-guarded idempotent data backfill touching only rows with a non-uuid parent_task_id; no hot-path write; tool schemas unchanged (blocked_by stays an array in the schema; string forms are tolerated input).

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
 internal/db/db.go                                  |   5 +
 internal/db/short_parent_backfill.go               | 108 +++++++++++++
 internal/db/short_parent_backfill_test.go          |  79 +++++++++
 internal/db/tasks.go                               |  38 ++++-
 internal/relay/api.go                              |   5 +
 internal/relay/handlers_tasks.go                   |  78 ++++++++-
 internal/relay/parent_dispatch_test.go             | 180 +++++++++++++++++++++
 8 files changed, 559 insertions(+), 9 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `98be27bb-2665-4582-a719-2e8d3cb3faf9--redreverify`._
