# [relay/W6] dispatch_task stores an optional linear_key on a native task

## Team : wraith-engine (tsukumo)
## Branch : wraith/16a2c671-w6-linear-key (from main)
## Relay task : 16a2c671-a21f-4ce0-80b2-2a802a00f922
## Trace : trace=8e0c33a4d496b374180cfe73062abedc
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: dispatch_task with linear_key 'SYN-123' then get_task returns linear_key 'SYN-123' and source 'native'
- [ ] 2. test: a linear_key not matching ^[A-Z][A-Z0-9]*-[0-9]+$ is refused with an error naming linear_key
- [ ] 3. test: a linear_key already held by an active task in the same project is refused, naming that task id
- [ ] 4. test: batch_dispatch_tasks accepts linear_key per item; dispatch without it is unchanged
- [ ] 5. go test -tags fts5 ./... green

## 2. Root cause & decisions

# 16a2c671 — W6: dispatch_task stores an optional linear_key on a native task

ROOT_CAUSE: dispatch_task / batch_dispatch_tasks had no linear_key argument and db.DispatchTask's INSERT never wrote the existing tasks.linear_key column (models.Task.LinearKey), so a native dispatch for work already tracked in Linear could not carry its issue key; only Linear-born mirrors had one.

DECISION:
- TypedTicket.LinearKey (dispatch-time plumbing, like verify_cmd) → written by the DispatchTask INSERT; source stays 'native'; no Linear write-back is triggered by the key alone.
- Validation at the single creation choke (db.DispatchTask, so single, batch, API and cron paths share it): format ^[A-Z][A-Z0-9]*-[0-9]+$ refused naming linear_key; a key already held by an active (not done/cancelled, not archived) task of the same project refused naming that task id.
- dispatch_task schema gains linear_key; batch_dispatch_tasks items accept linear_key (description lists it).

REJECTED ALTERNATIVES:
- A UNIQUE index on (project, linear_key): Linear mirrors and closed tasks legitimately share keys; the check is scoped to active tasks.

## review-wraith verdict: SHIP
Scope: internal/db/tasks.go, internal/relay/handlers_tasks.go, internal/relay/tools.go, internal/relay/dispatch_linear_key_test.go (4 files).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (via niwa slot run)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- the active-duplicate check reads before the INSERT (not in one tx): two simultaneous dispatches of the same key can both land; acceptable for a link field, no data loss.
Notes: no schema change (column exists); tool schema +1 optional string (toolsize tests green).
Tests: TestDispatchTask_StoresLinearKey, TestDispatchTask_LinearKeyFormatRefused, TestDispatchTask_LinearKeyHeldByActiveTaskRefused, TestBatchDispatch_LinearKeyPerItem.

RED_EVIDENCE:
  cmd: niwa slot run -- go test -tags fts5 ./internal/relay -run 'TestDispatchTask_StoresLinearKey|TestDispatchTask_LinearKey|TestBatchDispatch_LinearKeyPerItem'
  test_sha: 56ae594
  output: |
    --- FAIL: TestDispatchTask_StoresLinearKey (0.08s)
        dispatch_linear_key_test.go:35: get_task linear_key=<nil> source=native, want SYN-123 / native
    --- FAIL: TestDispatchTask_LinearKeyFormatRefused (0.10s)
        dispatch_linear_key_test.go:44: linear_key "syn-123": want a refusal naming linear_key, got ""
        dispatch_linear_key_test.go:44: linear_key "SYN": want a refusal naming linear_key, got ""
        dispatch_linear_key_test.go:44: linear_key "SYN-12a": want a refusal naming linear_key, got ""
        dispatch_linear_key_test.go:44: linear_key "123-SYN": want a refusal naming linear_key, got ""
        dispatch_linear_key_test.go:44: linear_key "SYN 123": want a refusal naming linear_key, got ""
    --- FAIL: TestDispatchTask_LinearKeyHeldByActiveTaskRefused (0.09s)
        dispatch_linear_key_test.go:57: duplicate linear_key: want a refusal naming linear_key and task 9599d768-b211-4d31-853a-659c1665aeaf, got ""
    --- FAIL: TestBatchDispatch_LinearKeyPerItem (0.08s)
        dispatch_linear_key_test.go:85: with key: linear_key "", want "SYN-9"

## 3. Files changed

```
internal/db/tasks.go                       | 37 +++++++++++-
 internal/relay/dispatch_linear_key_test.go | 95 ++++++++++++++++++++++++++++++
 internal/relay/handlers_tasks.go           |  4 +-
 internal/relay/tools.go                    |  3 +-
 4 files changed, 134 insertions(+), 5 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `16a2c671-a21f-4ce0-80b2-2a802a00f922`._
