# [relay/knowledge] consumption T2: recall capture, basis stamps on claim/complete, single who_consumed tool

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/consumption-t2 (from main)
## Relay task : ab5a9a77-8458-40cf-80c7-2d739ea4f82f
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 stamps: TestConsumptionTools/ClaimStampsHeadAndRecalls (boot, get_memory K, claim -> task_basis with head + recall_through containing K); /CompleteStampSeparateRow; /BatchCompleteOneTx; /StampFailureKeepsTransition (basis unknown, task transitioned).
- [ ] 2. AC2 who_consumed: /WhoConsumedFlagsOldVersion (A boots with K v1, K->v2, B boots -> A current=false, B current=true, A's claimed task in active_tasks); /WhoConsumedActiveOnly (inactive agent hidden by default, shown with active_only=false); /WhoConsumedSelfReturnsOwnBasis (self=true gives the caller's head + newer-version hint).
- [ ] 3. AC3 read-only + budget: /GetMemoryNoWriteOnCall (total_changes unchanged across 100 get_memory until the tick); /ToolsAreReadOnly (who_consumed delta 0); TestToolSchemaBudget green with bytes stated.

## 2. Root cause & decisions

ROOT_CAUSE: T1 records what each agent was served at boot, but no task recorded the knowledge it ran on, and nothing could answer "who is still working on version N of key K". Without that, a knowledge change has no path to the agents and tasks it affects.

DECISION:
- StampTaskBasis runs in its own writer tx after the transition (ruling OQ3). It drains the agent's buffered recalls into it, so recall_through is exact at stamp time. A failed stamp keeps the transition, returns basis "unknown" and puts the recalls back into the buffer.
- start_task is stamped only when it moves a task from pending (an implicit claim), detected with one RO read before the transition. batch_complete_tasks makes one stamp for the whole batch.
- The basis is added to the tool result through a JSON round-trip of the task, so every existing field is kept and models.Task is untouched.
- ONE tool, who_consumed(key, scope?, self?, active_only?) (ruling OQ5: no my_context_basis). self=true returns db.ContextBasis. active_tasks is computed per holder from task_basis claim rows whose head or recall snapshot holds a version of the key.
- Recall capture (get_memory, search_memory, recall_decisions) was already wired by T1, so it is not duplicated.

SCHEMA BUDGET: +569 B (80 tools, 54547 B, margin 3366 -> 2797 on this base). Together with knowledge S2 (+618 B) the margin becomes ~2179, only ~131 B above the 2048 headroom (flagged to wraith-cto for coherence T2).

REJECTED: a separate my_context_basis tool (ruling); stamping inside the transition tx (ruling OQ3); a `basis` column on tasks (taskColumns lockstep).

## review-wraith verdict: SHIP
Scope: internal/db/consumption.go, internal/relay/{handlers_tasks.go,handlers_consumption.go,toolset.go,handlers_consumption_test.go}
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -count=1 -tags fts5 -race OK (1148 passed, 12 packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- start_task from pending does one extra RO read (GetTask) before the transition. A racing claim in between only changes whether a claim stamp is written, never the transition.
- Single writer kept: one stamp tx per claim/complete (~30/day), with no write on the recall or who_consumed path (proven with data_version tests). agentColumns and taskColumns are untouched.

## 3. Files changed

```
internal/db/consumption.go                  | 136 +++++++++++-
 internal/relay/handlers_consumption.go      | 121 +++++++++++
 internal/relay/handlers_consumption_test.go | 318 ++++++++++++++++++++++++++++
 internal/relay/handlers_tasks.go            |  19 +-
 internal/relay/toolset.go                   |   1 +
 5 files changed, 592 insertions(+), 3 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ab5a9a77-8458-40cf-80c7-2d739ea4f82f`._
