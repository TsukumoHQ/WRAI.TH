# [relay/knowledge] S1: knowledge_log + clock written in every knowledge write tx

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/knowledge-s1 (from main)
## Relay task : 56eb821c-0de8-4408-9b41-5247db54b2d7
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 one row per write: TestKnowledgeLog/EveryWritePathLogsExactlyOneRow covers each K1 branch, K2..K6, row delta 1 per call, a rolled-back write (ErrBasedOnMismatch) leaves 0; /RevMonotonicAcrossProjects; /RememberDecisionOneTx: a forced failure after the retraction leaves the old decision live and 0 new rows (K6+K1 atomic).
- [ ] 2. AC2 change classes + clock: /EditorialDoesNotAdvanceClock, /NormalizedEqualForcesEditorialOverDeclaredBreaking, /LayerUpToConstraintsAtLeastBreaking, /LayerDownAtLeastNarrowing, /UndeclaredHighIsBreakingLowIsAdditive, /DeclaredBelowFloorRaisedAndDeclaredKept, /SiblingAndResolveAtLeastBreaking, /DeleteIsRetraction, /ValidityShrinkNarrowingWidenAdditive, /CausalColumnMatchesOpts, /ClockBumpsAllLowerDurabilities, /GlobalScopeUsesStarClock.
- [ ] 3. AC3 compaction + reads + migration: /CompactionKeepsLatestPerKeyAndBreakingRetraction, /CompactionRespectsMinLag, /DeltaCompactedFlagBelowWatermark, /DeltaStateCompleteAfterCompaction, /ReadPathsWriteNothing (total_changes unchanged across GetMemory, SearchMemory, KnowledgeDelta), /OldDBMigrates (log starts empty).

## 2. Root cause & decisions

ROOT_CAUSE: memory/decision writes left no durable, ordered record of what knowledge changed or how, so consumers could not ask "what changed since rev R" or tell a breaking change from an editorial one. And RememberDecision retracted the superseded decision and wrote its successor in two separate writes, so a crash between them left the old decision retracted with no successor.

DECISION: append one knowledge_log row (global AUTOINCREMENT rev, effective change class, live_ids) and bump knowledge_clock for every durability at or below the row's, inside the SAME writer tx as each write (K1 branches, validity, delete x2, resolve, decision archive). Fold RememberDecision K6+K1 into one tx (ruling OQ4). The log starts empty with no backfill (OQ3). Compaction keeps the latest row per key plus every breaking/narrowing/retraction row, so the log stays state-complete.

REJECTED:
- Triggers on memories: they can't see the declared class, the causal source or the op (sibling vs supersede).
- A separate log tx after commit: it opens a crash window between the write and its log row. Same tx or nothing.
- Backfilling from memory history: it would invent classes that nobody declared (OQ3).

Self-review fixes this round: validity narrowing now also covers a valid_from that appears (it used to cover only one that moves later). KnowledgeDelta clamps HeadRev to the highest rev it returns, because the head read and the row read are separate queries.

[LEGACY_OPPORTUNITY] Nothing calls CompactKnowledgeLog periodically yet. It needs a janitor hook in a later slice; until then the log grows with writes.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/{db.go,knowledge_log.go,knowledge_log_test.go,memories.go,decisions.go}
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 -race OK (1096 passed, 12 packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/knowledge_log.go:366 — nothing calls CompactKnowledgeLog periodically yet; it needs a janitor wiring slice.
- internal/db/memories.go:649 — whereArgs is sliced by counting "?" in where; correct today, but it breaks if the SET clause gains a placeholder after the where args.
- Single writer kept: every append rides the write's existing beginWriterTx (no new tx, no new writer). Migration is additive IF NOT EXISTS. Reads use d.ro().

## 3. Files changed

```
internal/db/db.go                 |   4 +
 internal/db/decisions.go          |  45 ++-
 internal/db/knowledge_log.go      | 399 +++++++++++++++++++++++++
 internal/db/knowledge_log_test.go | 613 ++++++++++++++++++++++++++++++++++++++
 internal/db/memories.go           | 214 +++++++++++--
 5 files changed, 1241 insertions(+), 34 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `56eb821c-0de8-4408-9b41-5247db54b2d7`._
