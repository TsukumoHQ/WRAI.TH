# [relay/budget] applyBudget scoring correctness: unknown priority, created_at parse, empty-tag renormalisation, selection journal

## Team : wraith-backend (tsukumo)
## Branch : feat/budget-scoring (from main)
## Relay task : 1f190795-0335-46c5-b800-5b9be5a21913
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1: a message with priority "" or any value outside P0-P3 scores exactly like P3 in utility() (never above a P1), and the final result order uses the priority index (P0<P1<P2<P3, unknown last), not string compare; pinned test covers "" vs P0 vs P1.
- [ ] 2. AC2: created_at is parsed with time.RFC3339Nano, falling back to the legacy layout 2006-01-02T15:04:05.000000Z; an unparseable created_at yields freshness 0; a future created_at yields age 0 (freshness 1); pinned tests for both cases.
- [ ] 3. AC3: when the agent has no interest_tags OR the message has no tags, utility uses weights 0.875 priority / 0.125 freshness (tag term dropped), so a fresh P0-scored message reaches 1.0; pinned test asserts the max score is 1.0 in the no-tag case.
- [ ] 4. AC4: every applyBudget call with >=1 candidate emits exactly one structured log line containing candidate ids, selected ids, per-message score and bytes, and budget used/max; pinned test asserts the line's fields.
- [ ] 5. AC5: no schema or migration change; go vet ./... clean; CGO_ENABLED=1 go test -tags fts5 -race ./internal/relay/... green.

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/relay/budget.go, internal/relay/budget_test.go (task 1f190795). Caller handlers_messaging.go untouched; apply_budget still off everywhere; mapPriority untouched.
Gate: build -tags fts5 OK / vet (tags + no tags) OK / gofmt OK / CGO_ENABLED=1 go test -tags fts5 -race ./internal/relay/... OK

AC1: priorityIndex() — P0..P3 -> 0..3, anything else -> 4; utility clamps unknown to 3 (== P3 score), final sort by index (unknown last). Tests: TestUtilityUnknownPriorityScoresAsP3, TestApplyBudgetOrdersByPriorityIndex ("" vs P0 vs P1).
AC2: freshness() parses RFC3339Nano, falls back to 2006-01-02T15:04:05.000000Z; unparseable -> 0, future -> age 0 -> 1. Tests: TestFreshnessParsing, TestUtilityUnparseableCreatedAtNotFresh.
AC3: no agent tags OR no msg tags -> 0.875*pri + 0.125*fresh; tagged path keeps 0.7/0.2/0.1. Test: TestUtilityNoTagRenormalisation (max no-tag score == 1.0).
AC4: one `[budget] {json}` log.Printf per call with >=1 candidate (deferred, covers all return paths incl. P0-over-budget and maxBytes<=0): candidates, selected, scores[{id,priority,score,bytes}], budget_used, budget_max. Tests: TestApplyBudgetJournalLine, TestApplyBudgetJournalEdgePaths (empty input -> 0 lines).
AC5: no schema/migration change; no new dependency.

Thesis checks: no DB write added (log only); P0 bypass unchanged; no inbox/delivery semantics touched; sorts now SliceStable (deterministic replay, no weight change).

BLOCKERS: none

NITS:
- journal logs only message ids (no content) — agent name absent since caller untouched; add via handlers_messaging.go:437 if replay needs per-agent grouping.

ROOT_CAUSE: budget.go scoring assumed well-formed input — utility() defaulted priIdx to 0 so any non-P0..P3 priority scored as P0 (1.0) minus the bypass; the final sort string-compared Priority so "" ranked before P0; freshness used one exact layout and treated parse failure / future dates as maximally fresh; the tag term was always 0 for untagged agents (all prod agents), capping utility at 0.8. Latent: apply_budget is off for the whole fleet.
DECISION: fix at scoring layer (priorityIndex + freshness helpers, proportional renormalisation when the tag term is undefined), plus a deferred single [budget] JSON journal line so every run is replayable before anyone enables apply_budget.
REJECTED: normalising priority in mapPriority (ticket forbids; send_message already maps unknown -> P2, scoring must still be robust to legacy rows); scoring an unparseable created_at as 0.5 (arbitrary, AC wants 0); separate journal sink/table (schema change, out of scope).

## 3. Files changed

```
internal/relay/budget.go      | 189 ++++++++++++++++++++++++++-----------
 internal/relay/budget_test.go | 215 ++++++++++++++++++++++++++++++++++++++++++
 2 files changed, 351 insertions(+), 53 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1f190795-0335-46c5-b800-5b9be5a21913`._
