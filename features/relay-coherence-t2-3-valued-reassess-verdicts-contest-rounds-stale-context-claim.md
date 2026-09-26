# [relay/coherence] T2: 3-valued reassess verdicts, contest rounds, STALE_CONTEXT claim fence for breaking constraints

## Team : wraith-engine (tsukumo)
## Branch : wraith/coherence-t2 (from main)
## Relay task : 0d6e5c37-0fb2-4fe4-9154-b8e669006fa6
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 verdicts + contests: TestCoherenceContest/UnaffectedClosesInactive; /ConflictOpensContradictionAgainstNewVersion (exception source_ref=new_memory_id, rollout contested); /UpheldReopensRound2; /OverturnedFixSupersedes; /SecondContestConstraintReachesHuman (only path to user, §5 schema); /SecondContestBehaviorStaysExecutive; /ReviewerNeverRaiserOrAuthorOrFounder.
- [ ] 2. AC2 fence: /EnforceFencesOnlyStaleBearers (unaffected agent claims fine; stale bearer gets STALE_CONTEXT; after get_memory + prepared it claims fine); /NarrowingNeverFences; /ContestedDropsFence; /GrandfatheredLeaseNotBlocked; advisory mode never refuses a claim and returns the stale list.
- [ ] 3. AC3 metrics + budget: /StaleBasisCompletionCounted (§7 query flags a completion on the old basis); A1 test from task_basis; TestToolSchemaBudget green with bytes stated.

## 2. Root cause & decisions

ROOT_CAUSE: coherence T1 could only say "prepared": an agent whose work a change does not touch had to fake a read, one who believed the new version was wrong had no way to say so (the contradiction stayed in prose), and under a breaking constraint nothing stopped a stale agent from claiming new work on the old version (design e731f3c9 §4.2, §5, §6; F-mas C1, E B8).
DECISION: obligation_discharge takes an optional verdict (prepared default | unaffected | conflict | upheld | overturned) + reason (+231 B schema, margin 3854 B). unaffected closes inactive (audited by the §7 stale-basis query). conflict (reason >= 10 chars) writes a knowledge_conflict/contradiction exception against the new version, sets the rollout contested (fence drops) and opens a coherence.review: round 1 on the author's reports_to else a live executive (never raiser, author or founder); round 2 on the human only for a constraints key (decide + §5 schema, 72h, once per rollout), else the executive. upheld re-opens the raiser's reassess one round deeper; overturned is refused until the fix is written and the fix's rollout supersedes. A lapsed review lets the new version stand. A1: leased tasks whose latest claim/reassess basis holds the old version. prepared on a task consumer re-stamps task_basis(event=reassess). Fence: ClaimTask/StartTask from pending refuse STALE_CONTEXT only in coherence_mode=enforce, only for a bearer of an open reassess on a breaking x constraints rollout that is preparing; leased tasks grandfathered; advisory returns stale_context on the claim/start result. Ruling 0019d6f5.
REJECTED: fence inside the CAS (ruled: pre-transition guard, the race self-heals into an A1 obligation); a new MCP tool for contests (the obligation tools carry the protocol); paging the human on every round-2 conflict (one open round-2 review per rollout).

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/coherence.go, internal/db/tasks.go (2 guard calls), internal/db/coherence_contest_test.go (new), internal/relay/tools.go (verdict + reason), internal/relay/handlers_obligations.go (verdict routing, notices, stale_context helper), internal/relay/handlers_tasks.go (2 call sites), internal/relay/coherence_verdict_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK; TestToolSchemaBudget 53259 -> 53490 B (+231), margin 3854 B

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- 7 files vs the ticket's 5: handlers_tasks.go (advisory stale_context on the claim/start result, 2 lines) and a relay-level verdict test were needed for the AC; both small.
- The fence reads coherence_mode + one indexed obligations query on every claim/start from pending (read pool only, no write).
- A retraction's contest is keyed "retraction:<old id>" (no new version to raise against).
- A lapsed review resolves the contest as expired and the new version stands (the design is silent on review deadlines).

## 3. Files changed

```
...-verdicts-contest-rounds-stale-context-claim.md |  55 ++
 internal/db/coherence.go                           | 684 +++++++++++++++++++--
 internal/db/coherence_contest_test.go              | 382 ++++++++++++
 internal/db/tasks.go                               |   6 +
 internal/relay/coherence_verdict_test.go           |  80 +++
 internal/relay/handlers_obligations.go             |  68 +-
 internal/relay/handlers_tasks.go                   |   4 +-
 internal/relay/tools.go                            |   3 +
 8 files changed, 1233 insertions(+), 49 deletions(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-0d6e5c37-0fb2-4fe4-9154-b8e669006fa6
- 🟢 AC1: all 7 sub-tests assert on observable state (exception source_ref=v2, rollout=contested, review depth/bearer/kind, schema.decision enum, executive fallback); mental revert of any branch fails the matching test — evidence: internal/db/coherence.go:282 DischargeCoherence + openContest/ruleContest implement 3-valued verdicts; internal/db/coherence.go:466-471 routes round-2 of constraints layer to user/human; internal/db/coherence.go:915-928 contestReviewer excludes author/raiser; nonAgentDispatchers[class_budgets.go:73] excludes founder/human — test: TestCoherenceContest/{UnaffectedClosesInactive,ConflictOpensContradictionAgainstNewVersion,UpheldReopensRound2,OverturnedFixSupersedes,SecondContestConstraintReachesHuman,SecondContestBehaviorStaysExecutive,ReviewerNeverRaiserOrAuthorOrFounder} coherence_contest_test.go:79-238
- 🟢 AC2: all 5 sub-tests assert fence true→STALE_CONTEXT, gate=advisory→no fence, contested→Fence=false, leased-then-supersede→no fence, advisory→claim succeeds + stale list — evidence: internal/db/coherence.go:754-773 guardStaleContext fences only enforce-mode + pending + gate=block_claims; tasks.go:621,659 wired into ClaimTask/StartTask; handlers_obligations.go:108-117 withStaleContext returns stale list in advisory without refusing — test: TestCoherenceContest/{EnforceFencesOnlyStaleBearers,NarrowingNeverFences,ContestedDropsFence,GrandfatheredLeaseNotBlocked,AdvisoryReturnsStaleList} coherence_contest_test.go:240-320
- 🟢 AC3: §7 query pins 1 stale completion (ta.ID), drops prepared bearer (tb.ID); A1 pins via=claim_basis and StampTaskID=task.ID on prepared; TestToolSchemaBudget prints 53490 bytes / 57344 cap — evidence: internal/db/coherence.go:778-803 StaleBasisCompletions §7 KPI; coherence.go:256-280 A1 query joins task_basis on snapshot/recall_through + consumption_edges memory_id; tools.go:518-519 verdict/reason enum — test: TestCoherenceContest/StaleBasisCompletionCounted coherence_contest_test.go:350; TestCoherenceContest/ClaimBasisConsumer coherence_contest_test.go:322; TestToolSchemaBudget relay/toolsize_test.go:37

## 5. Timeline

- round 1 → **approve** (review-0d6e5c37-0fb2-4fe4-9154-b8e669006fa6)

**Approve-with-findings (follow-up):** validate green; 15 TestCoherenceContest subtests pass; TestToolSchemaBudget 53490 bytes / 57344 cap; TestCoherenceVerdicts passes end-to-end; all ACs verified, fence pins claim/start STALE_CONTEXT, claim_basis consumer pins A1 via claim_basis + reassess stamp, StaleBasisCompletions pins §7 query

- **notice** `internal/db/coherence.go:917` — comment says never the raiser/author/founder/human, but bad() only filters raiser+author; founder/human are blocked via the nonAgentDispatchers map (class_budgets.go:73)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `0d6e5c37-0fb2-4fe4-9154-b8e669006fa6`._
