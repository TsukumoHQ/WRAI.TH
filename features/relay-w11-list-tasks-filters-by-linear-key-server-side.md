# [relay/W11] list_tasks filters by linear_key server-side

## Team : wraith-backend (tsukumo)
## Branch : wraith/c4f65348-w11-list-linear-key (from main)
## Relay task : c4f65348-9fa7-4202-88a5-1caa9ecb5173
## Trace : trace=1024d709a0cbd48de54eafb1fa78a3ac
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: list_tasks(linear_key='SYN-123') returns exactly the one task holding that key among 3 tasks
- [ ] 2. test: unknown linear_key returns 0 tasks, not the unfiltered list
- [ ] 3. test: linear_key combines with status filter (AND)
- [ ] 4. go test -tags fts5 ./... green

## 2. Root cause & decisions

# c4f65348 — W11: list_tasks filters by linear_key server-side

ROOT_CAUSE: list_tasks had no linear_key filter (schema, handler and db.ListTasks), and an unknown argument is not refused, so list_tasks(linear_key=…) silently returned the whole unfiltered board (field: 194,490 chars to find one task).

DECISION:
- db.ListTasksByKey = ListTasks + `AND linear_key = ?` in the same SQL (ANDs with status / profile / priority / assignee / board). ListTasks stays as the "" wrapper, so its 11 callers are unchanged.
- list_tasks schema gains linear_key; HandleListTasks passes it. ready=true + linear_key is refused (the ready listing is a different predicate; post-filtering in Go is out by the ticket).
- No new index: EXPLAIN QUERY PLAN on the live DB (3186 tasks) already uses idx_tasks_linear_key:
    SEARCH tasks USING INDEX idx_tasks_linear_key (linear_key=?)
    USE TEMP B-TREE FOR ORDER BY

REJECTED ALTERNATIVES:
- Adding a parameter to ListTasks: touches 6 files of callers for no behaviour change.

## review-wraith verdict: SHIP
Scope: internal/db/tasks.go, internal/relay/handlers_tasks.go, internal/relay/tools.go, internal/relay/list_tasks_linear_key_test.go (4 files).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (via niwa slot run)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none
Notes: read-only query on d.ro(); tool schema +1 optional string (toolsize tests green).
Tests: TestListTasks_LinearKeyFilter, TestListTasks_UnknownLinearKeyReturnsNone, TestListTasks_LinearKeyAndStatus.

RED_EVIDENCE:
  cmd: niwa slot run -- go test -tags fts5 ./internal/relay -run 'TestListTasks_LinearKey|TestListTasks_UnknownLinearKey'
  test_sha: 290b6db
  output: |
    --- FAIL: TestListTasks_LinearKeyFilter (0.06s)
    --- FAIL: TestListTasks_UnknownLinearKeyReturnsNone (0.06s)
        list_tasks_linear_key_test.go:48: unknown linear_key: got 3 task(s), want 0 (not the unfiltered list)
    --- FAIL: TestListTasks_LinearKeyAndStatus (0.05s)
        list_tasks_linear_key_test.go:58: linear_key AND status=pending on a cancelled task: got 2, want 0
    FAIL
    FAIL	agent-relay/internal/relay	0.500s
    FAIL

## 3. Files changed

```
...list-tasks-filters-by-linear-key-server-side.md | 83 ++++++++++++++++++++++
 internal/db/tasks.go                               | 11 +++
 internal/relay/handlers_tasks.go                   |  6 +-
 internal/relay/list_tasks_linear_key_test.go       | 63 ++++++++++++++++
 internal/relay/tools.go                            |  1 +
 5 files changed, 163 insertions(+), 1 deletion(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-c4f65348-9fa7-4202-88a5-1caa9ecb5173
- 🟢 AC1: Test exercises new SQL path via real handler; reverting the AND linear_key = ? clause returns all 3 tasks and breaks the assertion, so the test pins the fix. — evidence: internal/db/tasks.go:1387-1390 adds SQL filter AND linear_key = ?; internal/relay/handlers_tasks.go:1634,1648 wires trimmed input into ListTasksByKey — test: TestListTasks_LinearKeyFilter at internal/relay/list_tasks_linear_key_test.go:37-43 (fixture creates 3 tasks with SYN-1, SYN-123, and no key; asserts filter on SYN-123 returns exactly ids[two]). go test -tags fts5 -run TestListTasks_LinearKeyFilter -v ./internal/relay/ observed PASS 0.05s
- 🟢 AC2: Filter is exact equality; absent key produces zero rows rather than unfiltered list. — evidence: internal/db/tasks.go:1387-1390 makes unknown keys filter to zero rows because equality predicate fails; same query path as AC1 — test: TestListTasks_UnknownLinearKeyReturnsNone at internal/relay/list_tasks_linear_key_test.go:45-50 (fixture has 3 tasks, queries linear_key=SYN-999, asserts len==0). go test -tags fts5 -run TestListTasks_UnknownLinearKeyReturnsNone -v ./internal/relay/ observed PASS 0.06s
- 🟢 AC3: Both branches append AND to the same query, AND-combined; assertion would fail without the linear_key clause (pending would return 2 unfiltered tasks). — evidence: internal/db/tasks.go:1365-1370 (status branch) and 1387-1390 (linear_key branch) both append AND = ? to the same WHERE, AND-combining them; CancelTask preserves linear_key — test: TestListTasks_LinearKeyAndStatus at internal/relay/list_tasks_linear_key_test.go:52-63 (cancels the SYN-123 task, then queries linear_key=SYN-123&status=pending expect 0, and linear_key=SYN-123&status=cancelled expect 1). go test -tags fts5 -run TestListTasks_LinearKeyAndStatus -v ./internal/relay/ observed PASS 0.05s
- 🟢 AC4: Suite green; all packages report ok. — evidence: go test -tags fts5 ./... executed in review worktree; Go test: 1416 passed in 12 packages (matches daemon output) — test: Full suite; the three new tests in list_tasks_linear_key_test.go all pass under -tags fts5

### Round 2 — ❌ REJECTED by human:cto-tsukumo

## 5. Timeline

- round 1 → **approve** (review-c4f65348-9fa7-4202-88a5-1caa9ecb5173)
- round 2 → **reject** (human:cto-tsukumo)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c4f65348-9fa7-4202-88a5-1caa9ecb5173`._
