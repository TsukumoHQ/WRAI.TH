# [relay/exceptions] reason_code S1: block_task and cancel_task accept an optional closed reason_code that overrides the lexicon

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/313a2c8a-reason-code (from main)
## Relay task : 313a2c8a-a08c-44bb-aebf-f8db74f1a908
## Trace : trace=7b2560e06a61163579c58e1685d93c99
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. block_task or cancel_task with a reason_code outside the 10 values returns INVALID_ARGUMENT and the task status and exceptions rows are unchanged (test)
- [ ] 2. a declared reason_code sets exceptions.reason_code, kind and retry_class from the section 2 table and reason_source='declared' on block, on cancel-from-blocked and on cancel of a pending task, even when the lexicon would pick another code from the text (test per path)
- [ ] 3. cancel_task with a reason_code and no reason text writes one exception row with the declared code and leaves tasks.blocked_reason NULL (test)
- [ ] 4. with no reason_code the exception rows match today's lexicon output except reason_source='lexicon'; existing TestExceptions tests pass unmodified
- [ ] 5. go test -tags fts5 ./... green including TestToolSchemaBudget, total grows by at most 354 B

## 2. Root cause & decisions

ROOT_CAUSE: block_task / cancel_task only took free text, so every task_block / task_cancel exception got its reason_code from the reasonLexiconV1 regex guess (22% unclassified on the live relay, with misfires in the automation path, e.g. a founder-only credential block filed as benign operator_override). Design dbc317f4 slice 1, ruling aa630ca6.

DECISION:
- Optional `reason_code` on block_task + cancel_task (tools.go reasonCodeParam, enum from db.DeclaredReasonCodes, no description): the 10 ruled codes, each with its kind + retry_class (exceptions.go declaredReasonClass, shaped like DeclineReasons).
- Unknown value: validationError(CodeInvalidArgument) in the handler, before resolveTaskID or any write; the db layer also refuses it (defence in depth).
- A declared code fills exceptionOpen.Code/Kind/Retry + Declared, so openExceptionTx skips the lexicon. reason_source is set centrally there: declared | lexicon (no code, lexicon ran) | producer (a machine producer passed its own code). Producer call sites are untouched.
- exceptions.reason_source TEXT NULL via ensureColumns in migrateExceptions, before the backfill; no default, no index.
- writeTransitionExceptions takes reasonCode:
  - block open uses it;
  - cancel-from-blocked feeds it to the block row's resolution_reason AND writes a task_cancel row carrying the declared code (AC2 requires the declared kind/retry/reason_source on that path; without a code the path is unchanged, still no cancel row);
  - cancel-of-non-blocked uses it;
  - a code with no text still writes the row (the needExc gate's hasReason includes the code).
- BlockTask / CancelTask keep their signatures and delegate to new BlockTaskWithCode / CancelTaskWithCode. Changing the signatures would have touched api.go and 9 test files (outside the 5-file scope); REST is slice 2.
- No tasks column; taskColumns/scanTask/models untouched; same writer tx; no grouping_version bump.
- Tool schemas 54100 -> 54454 B (+354 exactly, as budgeted), margin 2890 to 57344.

REJECTED:
- Overwriting the open block row's reason_code on a declared cancel: it rewrites history.
- A `reason_code *string` through transitionTask's 10 callers: "" as none is enough and keeps every other transition byte-identical.

KNOWN GAP (flag, not fixed; outside the 5 files): class_budgets.go:380 inserts the class_budget systemic row directly (not via openExceptionTx), so those rows keep reason_source NULL instead of 'producer'. Fix is a one-column add there; it can ride lexicon v2 fbfa4b47.

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/... -run ReasonCode
  test_sha: 2ab0917
  output: |
    # agent-relay/internal/db [agent-relay/internal/db.test]
    internal/db/exceptions_test.go:848:17: d.BlockTaskWithCode undefined (type *DB has no field or method BlockTaskWithCode)
    internal/db/exceptions_test.go:874:17: d.CancelTaskWithCode undefined (type *DB has no field or method CancelTaskWithCode)
    internal/db/exceptions_test.go:906:17: d.CancelTaskWithCode undefined (type *DB has no field or method CancelTaskWithCode)
    --- FAIL: TestReasonCode_HandlersRefuseUnknownCode (0.08s)
        handlers_tasks_test.go:394: expected an error result, got success/nil
    FAIL	agent-relay/internal/relay	0.601s
    FAIL

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/exceptions.go, internal/db/tasks.go, internal/relay/tools.go, internal/relay/handlers_tasks.go, internal/db/exceptions_test.go, internal/relay/handlers_tasks_test.go
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 ./... OK (TestExceptions* unmodified + green; TestToolSchemaBudget 54454 B, +354, margin 2890)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/class_budgets.go:380: the systemic row insert bypasses openExceptionTx, so its reason_source stays NULL instead of 'producer'. Needs a one-column add in a file outside this ticket's scope; proposed to ride fbfa4b47.

Checked:
- single writer (all writes stay in the transition's writerTx; ensureColumns runs at migrate time only, before the backfill)
- additive nullable column (old binary ignores it, new binary adds it)
- CAS transition unchanged (the code is validated before GetTask/UPDATE)
- no taskColumns/agentColumns/scan change
- no new tool; stdio and HTTP share the registry
- no-code path byte-identical except reason_source='lexicon'

## 3. Files changed

```
internal/db/exceptions.go             |  87 ++++++++++++---
 internal/db/exceptions_test.go        | 197 ++++++++++++++++++++++++++++++++++
 internal/db/tasks.go                  |  28 ++++-
 internal/relay/handlers_tasks.go      |  23 +++-
 internal/relay/handlers_tasks_test.go |  34 ++++++
 internal/relay/tools.go               |  13 ++-
 6 files changed, 363 insertions(+), 19 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `313a2c8a-a08c-44bb-aebf-f8db74f1a908`._
