# [wraith/relay] update_task typed-field validation: native-array acceptance_criteria coerced, wrong types refused loudly (silent drop today)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/update-task-typed-fields (from main)
## Relay task : 88510cb5-309d-478c-b60e-b8311aee2eb9
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: update_task with acceptance_criteria as a native array of strings applies — stored value equals the canonical JSON-string form, and a progress_note in the same call is applied too (the 3ad80aae repro shape)
- [ ] 2. AC2 named test: update_task with a wrong-typed value on a string field refuses INVALID_ARGUMENT naming the field and expected type; no field from that request is applied (atomic refusal, last_activity_at unchanged)
- [ ] 3. AC3 named test: acceptance_criteria as an array containing a non-string element refuses INVALID_ARGUMENT naming the field — not coerced, not dropped

## 2. Root cause & decisions

ROOT_CAUSE: HandleUpdateTask (internal/relay/handlers_tasks.go) read every field via req.GetString, which silently falls back to "" when an arg is present but the wrong JSON type. A native-array acceptance_criteria therefore resolved to "" -> optionalString -> nil and was DROPPED with a 200 + bumped last_activity_at, no error (live repro 3ad80aae); a mistyped value on any other string field was likewise dropped. batch_dispatch_tasks accepts real arrays for the same field, making the asymmetry a trap. FIX: a typed-field validation pass right after the unknown-key check, BEFORE any DB write — (1) every string field present with a non-string value is refused INVALID_ARGUMENT naming the field + expected type; (2) acceptance_criteria additionally accepts a native []string, coerced to its canonical JSON-string form (json.Marshal) for batch_dispatch parity; (3) an AC array with a non-string element is refused, not coerced/dropped. Validation precedes UpdateTaskFields/reassign/AddProgressNote, so a refusal is atomic (no partial apply, last_activity_at unchanged).

## review-wraith verdict: SHIP
Scope: internal/relay/handlers_tasks.go (HandleUpdateTask typed-field pass), internal/relay/update_task_typed_fields_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 -count=1 OK (832 pass, full ./...)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- No schema/migration, no new writer, no new tool/route — validation-only in an existing handler; middleware/auth/permission model untouched. Backward-compatible: a JSON-string acceptance_criteria keeps the existing json.Unmarshal array validation (existing TestUpdateTask_DispatcherRescopesContract_Audited still green). Atomicity proven by AC2 (sibling valid field + last_activity_at unchanged on refusal). Failure-is-loud satisfied: AC2/AC3 assert the refusal message names the field.
- DoD note: after the next agent-relay redeploy, the niwa memory key `niwa-update-task-ac-must-be-json-string` (the string-only workaround) is obsolete and should be retired by the dispatcher.

## 3. Files changed

```
internal/relay/handlers_tasks.go                |  50 ++++++++-
 internal/relay/update_task_typed_fields_test.go | 130 ++++++++++++++++++++++++
 2 files changed, 179 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `88510cb5-309d-478c-b60e-b8311aee2eb9`._
