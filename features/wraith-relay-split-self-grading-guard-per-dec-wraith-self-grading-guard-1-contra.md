# [wraith/relay][split] self-grading guard per DEC-wraith-self-grading-guard-1: contract edits on self-dispatched tasks need reports_to sign-off

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/self-grade (from main)
## Relay task : 0cabf3f4-f7ea-4d28-aa83-f1a3e48eef53
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 update_task on contract fields where dispatcher==assignee without sign-off is refused with an explicit error naming the self-grading rule (bf920f6c reproduction test, named)
- [ ] 2. AC2 same edit WITH reports_to (or board-owner fallback) sign-off succeeds (named test)
- [ ] 3. AC3 non-contract fields (status, result, labels) on self-dispatched tasks remain freely editable (named test)
- [ ] 4. AC4 go test -tags fts5 ./... green, full suite

## 2. Root cause & decisions

# 0cabf3f4 — self-grading guard (DEC-wraith-self-grading-guard-1)

## ROOT_CAUSE
`HandleUpdateTask`'s contract-field guard (handlers_tasks.go) gated edits to
`goal/acceptance_criteria/dod/verify_cmd` on `agent == existing.DispatchedBy`.
On a **self-dispatched** task the dispatcher IS the doer
(`DispatchedBy == AssignedTo`), so the doer passed the check freely and could
rewrite its own grading bar — live finding bf920f6c: a self-dispatched doer cut
his own acceptance_criteria 5→2, and when his lead tried to restore them the
relay REFUSED ("can only be updated by this task's dispatcher") because the doer
was the dispatcher. A self-dispatched contract was an unguarded self-graded
contract.

## FIX
In the contract-field guard, detect self-dispatch
(`existing.AssignedTo != nil && EqualFold(existing.DispatchedBy, *existing.AssignedTo)`).
- Self-dispatched → the doer alone cannot edit contract fields; require sign-off
  from ABOVE the doer via new helper `callerIsContractSigner`: an executive, or
  an agent in the doer's `reports_to` lead chain. Else refuse loud (CodeForbidden,
  names the self-grading rule + DEC-wraith-self-grading-guard-1).
- Not self-dispatched → unchanged (`agent != DispatchedBy` → refuse).
`callerMayReassign` refactored to `dispatcher-shortcut || callerIsContractSigner`
(identical behaviour, DRY — the executive+lead-chain walk now lives in one place).

Board-owner fallback (parenthetical in the DEC) is intentionally omitted: the
relay's `Board` has no owner column (only `created_by`) and no by-id accessor,
and the DEC's core intent — "a superior signs, never the doer" — is fully met by
executive-or-lead-chain. A top-level self-dispatcher with no superior simply
cannot self-rewrite its contract, which is the correct safety property. Keeps the
change to ONE non-test source file, zero schema/tool-schema change.

## SCOPE
- internal/relay/handlers_tasks.go (+32/-3) — 1 non-test source file
- internal/relay/self_grading_guard_test.go (new) — 3 tests

## VERIFY
- go build ./... OK; go vet -tags fts5 ./... clean
- go test -tags fts5 ./... — all packages green (710 PASS subtests)
- TestToolSchemaBudget green, 51647 bytes UNCHANGED (no tool-schema change)
- AC1 TestUpdateTask_SelfDispatchedDoerCannotRescopeOwnContract
- AC2 TestUpdateTask_SelfDispatchedContractNeedsSignoffFromAbove (lead + executive)
- AC3 TestUpdateTask_SelfDispatchedFreeFormFieldsStillEditable

## review-wraith verdict: SHIP
Scope: internal/relay/handlers_tasks.go (self-grading contract guard + callerIsContractSigner helper, callerMayReassign DRY refactor), internal/relay/self_grading_guard_test.go (new, 3 tests)
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (all packages green, 710 PASS subtests; TestToolSchemaBudget green @ 51647 bytes UNCHANGED)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none. Permission-only change: no DB write added, no schema/migration, no messaging/auth/ingest/SSE/updater surface touched, no tool/route added. GetTask read was already present (not new). callerMayReassign refactor is behaviour-identical (dispatcher-shortcut || executive || lead-chain). Executive/lead-chain reads tolerate nil/err. Single-lane, in-lane, cannot red trunk.

## 3. Files changed

```
internal/relay/handlers_tasks.go          |  35 ++++++++-
 internal/relay/self_grading_guard_test.go | 117 ++++++++++++++++++++++++++++++
 2 files changed, 149 insertions(+), 3 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `0cabf3f4-f7ea-4d28-aa83-f1a3e48eef53`._
