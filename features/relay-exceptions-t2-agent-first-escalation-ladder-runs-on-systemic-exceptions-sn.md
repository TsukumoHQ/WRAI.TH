# [relay/exceptions] T2: agent-first escalation ladder runs on systemic exceptions (snapshot, CAS cursor, human inquiry last)

## Team : wraith-engine (tsukumo)
## Branch : wraith/budgets-t2 (from main)
## Relay task : ced5ffc7-b463-4f7c-b5b6-a05aa47230c1
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 ladder integrity: TestExceptionLadder/SnapshotFrozen; /InvalidLadderInert (human not last -> row disabled + audit event); /CASDoubleAdvance (one rung+1, one child obligation); /HumanOnlyAtMaxDepth (escalation_depth == len(snapshot)-1, only after all agent rungs closed); /OwnerNoneRoutesToLiveSupervisor (dead-project systemic never targets user or an inactive agent).
- [ ] 2. AC2 safety trims: /UpstreamTrim (attempts_sum=6 -> no ask_source; route_specialist never targets a reviewer profile); /NoGateRerun (reversible_action naming qa-submit refused at snapshot); /ActionIdempotentAfterCrash (no second task by tag); hard-stop at 2x total_attempt_budget routes to supervisor.
- [ ] 3. AC3 resolution paths: /RouteSpecialistDoneResolves (done -> resolved_by=peer, fixed; cancelled advances); /AskSourceFixedResolves; /HumanSchemaValidated (missing expires_in -> one re-ask; accept_known sets suppressed_until; TTL passed -> resolved_by=expired); /AttemptsView lists every rung outcome. Mode shadow/off: evaluateExceptionLadders does nothing.

## 2. Root cause & decisions

ROOT_CAUSE: systemic exceptions (T1) were recorded but nothing acted on them: without a ladder, a breached class either waited for a human to notice or would have been escalated ad hoc per instance, with no attempt history and no guarantee the human comes last (design 1111292b §4, D-exceptions B3/B4/B8).
DECISION: a ladder frozen at start (snapshot JSON on the exception), rungs as obligations on the existing engine with CAS cursor; rung actions reuse DispatchTask and InsertMessageWithDeliveries with an idempotency tag; Niwa upstream trims + 2x-budget hard stop; live-only targets with a fleet-wide executive fallback for dead projects; human inquiry validated from reply metadata (no new tool). Runs only when class_budget_mode=on.
REJECTED: opening answer obligations on ladder messages (the answer chain escalates to answer.human, i.e. the human before max depth); a resolve_exception MCP tool (schema budget; ruled against); a second sweeper (the ACK tick already hosts obligation sweeps).

## review-wraith verdict: SHIP
Scope: internal/db/exception_ladder.go (new), internal/relay/exception_ladder.go (new), internal/relay/cleanup.go (tick hook), tests internal/db/exception_ladder_test.go + internal/relay/exception_ladder_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- human "reassign" resolves the systemic with resolution_reason=reassigned and records reassign_to; it does not re-open route_specialist on the new owner (a follow-up if wanted).
- accept_known suppression is per (kind, reason_code) row, not per project (class_budgets has no project column).
- rung task titles start with "[exc <id8> r<n>]"; a project with a strict title guard may refuse them; the rung then fails at its deadline and the ladder moves on (dispatch_failed; this fallback has no dedicated test).
- cost_tokens stays NULL (ruled).

## 3. Files changed

```
internal/db/exception_ladder.go         | 699 ++++++++++++++++++++++++++++++++
 internal/db/exception_ladder_test.go    | 211 ++++++++++
 internal/relay/cleanup.go               |   5 +-
 internal/relay/exception_ladder.go      | 484 ++++++++++++++++++++++
 internal/relay/exception_ladder_test.go | 309 ++++++++++++++
 5 files changed, 1706 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ced5ffc7-b463-4f7c-b5b6-a05aa47230c1`._
