# [relay/norms] obligations slice 2b: obligations_mine, obligation_discharge, obligation_decline MCP tools

## Team : wraith-engine (tsukumo)
## Branch : wraith/obligations-s2b (from main)
## Relay task : 79f48b9e-ed50-45db-bfff-34730a857de4
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. obligations_mine returns the caller's active obligations (direct or via its profile pool) with subject, what, deadline and escalation depth (test ObligationsMineIncludesPool).
- [ ] 2. obligation_discharge re-checks the obligation's predicate and moves it to fulfilled only if it holds; otherwise it returns an error and the state is unchanged (tests DischargeRechecksPredicate, DischargeRefusedWhenPredicateFalse).
- [ ] 3. obligation_decline requires a reason_class from a closed enum and records it; the obligation then follows its norm's decline path (test DeclineRequiresReasonClass).
- [ ] 4. The three tools fit the tool-schema budget (TestToolSchemaBudget green); session_context output is unchanged; go vet clean; go test -tags fts5 -race ./internal/relay/... green.

## 2. Root cause & decisions

# 79f48b9e — obligations slice 2b: obligations_mine / obligation_discharge / obligation_decline

ROOT_CAUSE: after slices 1/2a the relay holds each agent's obligations but agents could not see them, and the only way out of one was to claim the task or wait for the escalation.

DECISION (design 84f68979 §4.2; details in the commit body):
- obligations_mine: RO; the caller's profile pool (bearer = name or profile) plus tasks assigned to it; subject, what, deadline (norm setting from dispatched_at), escalation depth.
- obligation_discharge: one writer tx re-checks the predicate (task_left_pending: left pending, not cancelled) before CASing active->fulfilled; a false predicate is refused and nothing changes.
- obligation_decline: closed reason_class enum; bearer only (FORBIDDEN otherwise); the rung breaches now through the same transition as a due rung (decline_reason_class recorded) and its sanction is sent immediately with the reason. ackSanction/sendAckSanction extracted from the sweeper so both paths share routing (the sweep's behaviour is unchanged; chain + equivalence tests green).

## review-wraith verdict: SHIP
Scope: internal/db/obligations.go, internal/relay/{handlers_obligations.go,tools.go,toolset.go,cleanup.go,handlers_obligations_test.go,toolset_test.go}.
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/... 0 FAIL (2 runs, rebased on d215a0c); TestToolSchemaBudget green (margin 3366 B); discovery payload per category under 16000 B.

BLOCKERS: none.
- mine is read-only; discharge/decline are single writer txs with CAS on state='active'.
- Handlers go through resolveProject/resolveAgent; tools registered once in toolset.go (tasks category).
- session_context unchanged (SessionContextUnchanged).

NITS (non-blocking):
- 7 files vs 5: toolset.go (registration), cleanup.go (sanction extraction), toolset_test.go (pinned tasks count 22 -> 25).
- Tool schema margin now 3366 B; the next tool addition may need trimming or a deliberate cap raise.

## 3. Files changed

```
internal/db/obligations.go                  | 161 +++++++++++++++++++++++++-
 internal/relay/cleanup.go                   |  80 +++++++------
 internal/relay/handlers_obligations.go      | 112 ++++++++++++++++++
 internal/relay/handlers_obligations_test.go | 171 ++++++++++++++++++++++++++++
 internal/relay/tools.go                     |  32 ++++++
 internal/relay/toolset.go                   |   3 +
 internal/relay/toolset_test.go              |   5 +-
 7 files changed, 528 insertions(+), 36 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `79f48b9e-ed50-45db-bfff-34730a857de4`._
