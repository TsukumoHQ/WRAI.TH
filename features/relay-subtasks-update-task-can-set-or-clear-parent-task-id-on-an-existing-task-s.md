# [relay/subtasks] update_task can set or clear parent_task_id on an existing task, so live tickets can be grouped under an epic

## Team : wraith-backend (tsukumo)
## Branch : wraith/eae0243b-update-parent (from main)
## Relay task : eae0243b-d987-4ec1-acef-634a876ac46e
## Trace : trace=d2c8814bc4422335ae316496771c1d41
## Status : 🔵 IN REVIEW

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: update_task(parent_task_id=P) on T -> get_task(P, include_subtasks) lists T; '' clears it
- [ ] 2. test: cycle refused with INVALID_ARGUMENT; self-parent refused; parent in another project refused
- [ ] 3. test: non-dispatcher agent refused; the change leaves an audit progress note
- [ ] 4. test: short-id prefix for parent resolves; unknown id -> NOT_FOUND
- [ ] 5. go test -tags fts5 ./... green

## 2. Root cause & decisions

# eae0243b update_task parent_task_id

ROOT_CAUSE: parent_task_id was only writable at dispatch (internal/relay/handlers_tasks.go:120 HandleDispatchTask, tools.go dispatch schema); update_task's whitelist (handlers_tasks.go updateTaskArgs) refused it as an unknown field and no DB path rewrote tasks.parent_task_id, so live tickets could not be regrouped under an epic without recreating them.

FIX: update_task accepts parent_task_id (string; id or short prefix resolved like task_id; "" detaches). Handler updateParent: GetTask, refuse Linear mirrors (parent syncs from Linear), authority = contractEditAuthority (dispatcher / executive / lead chain, never the doer; per ticket "same auth as goal/AC edits"), then db.SetTaskParent. SetTaskParent runs in one writer tx: read old parent, no-op if unchanged, refuse self (ErrParentSelf), parent missing in this project (ErrParentNotFound -> NOT_FOUND; covers another project), ancestor walk from the new parent reaching the task (ErrParentCycle -> INVALID_ARGUMENT; walk bounded at 64 against corrupt loops), then UPDATE parent_task_id + last_activity_at and INSERT the progress note "parent set to X by Y" / "parent cleared (was X) by Y". Check and write share the single writer conn, so two concurrent edits cannot jointly make a cycle. Nothing inherited. Docs: tools.go schema, skill/relay.md one line, skill/tools-reference.md.

INTERPRETATION: "no cycles (walk ancestors, depth cap 3)" read as a bounded ancestor walk for cycle detection, not a new nesting limit (a cap of 3 on the walk would miss longer cycles; no AC tests a depth limit). UNCERTAIN: if a max nesting depth of 3 is wanted, it is a follow-up guard.

## review-wraith verdict: SHIP
Scope: internal/db/task_parent.go (new), internal/relay/handlers_tasks.go (updateParent + whitelist), internal/relay/tools.go (schema), skill/relay.md, skill/tools-reference.md, internal/relay/parent_update_test.go (+ Linear-mirror refusal test)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK / -race Parent|UpdateTask OK

BLOCKERS (must fix before merge):
- none. Single writer: SetTaskParent uses beginWriterTx; no schema/migration; no hot-path write; agentColumns untouched; one registry tool schema extended (stdio + HTTP share it); refusals land before any write.

NITS (non-blocking):
- none

RED_EVIDENCE:
cmd: go test -tags fts5 -count=1 ./internal/relay/ -run TestUpdateTaskParent
test_sha: 928e823
note: the daemon re-runs cmd at test_sha^. Tests were committed first (8a522e9, red: update_task refused parent_task_id). test_sha is the fix commit 928e823 (fix + the Linear-mirror refusal test), so test_sha^ = 8a522e9 is exactly the test-first tree where cmd fails; at the old test_sha^ (ecb50bd) the new tests did not exist yet, so -run matched nothing and passed.
output (at 8a522e9 = 928e823^, exit 1):
FAIL TestUpdateTaskParent_SetListsAndClear
     parent_update_test.go:79: set parent: {"code":"INVALID_ARGUMENT","errorCategory":"validation",... "parent_task_id" is not an updatable field of update_task
FAIL TestUpdateTaskParent_RefusesSelfCycleAndOtherProject
     parent_update_test.go:111: seed chain: {"code":"INVALID_ARGUMENT",...
FAIL TestUpdateTaskParent_NonDispatcherRefusedAndAudited
     parent_update_test.go:143: non-dispatcher: want FORBIDDEN, got isErr=true {"code":"INVALID_ARGUME...
FAIL TestUpdateTaskParent_ShortIDAndUnknown
     parent_update_test.go:174: short-id parent: {"code":"INVALID_ARGUMENT",...
FAIL	agent-relay/internal/relay

## 3. Files changed

```
...r-clear-parent-task-id-on-an-existing-task-s.md |  72 ++++++++
 internal/db/task_parent.go                         | 101 ++++++++++
 internal/relay/handlers_tasks.go                   |  55 +++++-
 internal/relay/parent_update_test.go               | 203 +++++++++++++++++++++
 internal/relay/tools.go                            |   1 +
 skill/relay.md                                     |   1 +
 skill/tools-reference.md                           |   2 +-
 7 files changed, 431 insertions(+), 4 deletions(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-eae0243b-d987-4ec1-acef-634a876ac46e
- 🟢 AC1: SetTaskParent internal/db/task_parent.go:31 writes parent_task_id and inserts task_progress_notes note in one writer tx; updateParent internal/relay/handlers_tasks.go:1467 wired into HandleUpdateTask at 1397 — test: TestUpdateTaskParent_SetListsAndClear internal/relay/parent_update_test.go:75 — sets epic parent, asserts subtask IDs = [child], asserts task.ParentTaskID == epic; clears with empty, asserts subtask list empty
- 🟢 AC2: SetTaskParent internal/db/task_parent.go:50-79 walks parent chain to detect cycle (ErrParentCycle), self-parent (ErrParentSelf), other-project (ErrParentNotFound — parent not in this project); updateParent handlers_tasks.go:1491-1496 maps both to CodeInvalidArgument — test: TestUpdateTaskParent_RefusesSelfCycleAndOtherProject internal/relay/parent_update_test.go:103 — 4 sub-cases: self-parent, direct cycle, cycle through chain (a<-b<-c, set a=c), parent in another project (p2 task from p1) all return correct code; also verifies refused edit wrote nothing
- 🟢 AC3: updateParent handlers_tasks.go:1479 uses contractEditAuthority (dispatcher/exec/lead chain only); SetTaskParent task_parent.go:82-96 inserts task_progress_notes row 'parent set to {parentID} by {actor}' on success — test: TestUpdateTaskParent_NonDispatcherRefusedAndAudited internal/relay/parent_update_test.go:138 — dev-x (non-dispatcher) refused FORBIDDEN; refused edit wrote nothing; then lead (dispatcher) sets parent; GetProgressNotes returns 'parent set to {epic} by lead'
- 🟢 AC4: updateParent handlers_tasks.go:1485 calls resolveTaskID (handles short prefix via ResolveTaskID internal/db/tasks.go:1884 — LIKE prefix% match); unknown id flows to SetTaskParent which returns ErrParentNotFound mapped to CodeNotFound handlers_tasks.go:1491 — test: TestUpdateTaskParent_ShortIDAndUnknown internal/relay/parent_update_test.go:170 — epic[:8] resolves and sets parent correctly; 00000000-... (valid UUID form, no row) refused NOT_FOUND
- 🟢 AC5: rtk proxy go test -tags fts5 ./... exit 0; all 13 packages ok including agent-relay/internal/relay 117.957s, agent-relay/internal/db 104.428s, agent-relay/internal/connector/linear 19.785s — test: N/A — gate-level command, not a behavioral test

## 5. Timeline

- round 1 → **approve** (review-eae0243b-d987-4ec1-acef-634a876ac46e)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `eae0243b-d987-4ec1-acef-634a876ac46e`._
