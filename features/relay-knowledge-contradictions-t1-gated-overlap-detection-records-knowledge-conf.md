# [relay/knowledge] contradictions T1: gated overlap detection records knowledge_conflicts and precedence edges, with a write-time supersede hint

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/contradictions-t1 (from main)
## Relay task : 461ec7c6-b30f-493d-ae0a-c232d19c8b82
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 gates: TestContradictions/PG151617NoConflict (0 conflicts, 3 implicit edges M2->M1, M3->M1, M3->M2); /SameRankSameLayerDisagreeIsCandidate; /DisjointValidityIsAmends; /BehaviorContextNeverConflict; /KeyFamilyAloneNotSubject; /DeclaredSubjectMatches.
- [ ] 2. AC2 sweep + hint: /DuplicateGroupOnce (re-run and concurrent ticks -> one row); /WriteTimeHintNoWrite (overlaps + suggestion returned, total_changes for the D1 read = 0, write succeeds); /ClaimLeaseCAS; /TwoFailedClaimsEscalateToException (exception kind=contradiction, no message to user).
- [ ] 3. AC3 prod replay: /ReplayProd on a read-only COPY of a prod backup: detector hits include the 5 same-fact pairs of design §1; PR body lists them.

## 2. Root cause & decisions

ROOT_CAUSE: the fleet re-writes a fact, or an update of it, under a new key. Both copies stay live, and a later supersede of one copy never reaches the consumers of its twin (design 8d107daa §1, cto 0019d6f5 OQ7). Nothing detected cross-key overlap, and conflict_with was never written (0 rows).

DECISION:
- internal/db/contradictions.go (new):
  - Schema §4.1: knowledge_edges (WITHOUT ROWID, PK natural key) and knowledge_conflicts (members_hash UNIQUE, partial open index). memories.invalidated_by is added via ensureColumns; memorySelectCols is untouched.
  - fts5vocab over memories_fts, for the rarest-token pick.
  - The D2 cursor starts at the knowledge_log head.
- The gates are pure functions over columns:
  - G1 (any one of these holds): the tag `subject:<x>` is equal; or the decision area is equal ("general" excluded: DEC-general-1..9 share it and nothing else); or the peer is in the FTS5 BM25 top-5 of N's 12 rarest value tokens (value column only) AND word-Jaccard >= contradiction_overlap_min (0.35, OQ1). The key string is never a signal.
  - For decisions, G1 compares decision + rationale, not the raw JSON. The JSON field names made every decision pair overlap: KeyFamilyAloneNotSubject fails without this.
  - G2: extents must meet; a declared overrides/amends edge exempts the pair; a different scope rank gives an implicit overrides edge (narrower -> broader).
  - G3: disjoint windows give an amends edge (older -> newer).
  - G4: only constraints/decision are considered; decision x constraint gives overrides (decision -> constraint); same layer is a candidate.
  - Implicit edges are recorded with declared=0 (OQ2).
- D1: OverlapHint on the RO pool (mode=ro). set_memory runs it when the outcome is fresh and the layer is doctrine; remember runs it without supersedes. The result gains overlaps [{key, scope, memory_id, similarity, relation}] and suggestion. The write is never refused. Off when contradiction_mode=off.
- D2: EvaluateContradictions runs on the ACK tick (contradiction_mode off|detect, default detect). In order:
  1. lease expiry (failed_claims+1);
  2. escalation at 2 failed claims: state CAS, then openExceptionTx kind=contradiction, source=contradiction, in the same tx. The class-budget ladder owns it (OQ7), and no message goes to user;
  3. the one-shot audited backfill (OQ3, marker contradiction_backfill_v1, every recorded pair logged);
  4. the cursor sweep over set/supersede/sibling/conflict HIGH rows. Each hit is one writer tx that carries the cursor CAS.
- ClaimConflict / ReleaseConflict are lease CAS DB functions. T2 exposes them as tools (not in this ticket: no tools.go change, and the schema bytes are unchanged at 54483).

REPLAY (AC3) on a COPY of ~/.agent-relay/backups/relay.20260926T101106Z.pre-7d5683f.db (NewTestDB on a temp copy; prod untouched): 10 conflicts + 2 edges. All 5 same-fact pairs of §1 are detected:
- edge overrides qa-submit-niwa-exe-must-be-dot-niwa-src-binary (project) -> same key (global) (scope_rank)
- edge overrides DEC-general-4 -> niwa-hook-live-patch-lost-on-provision (layer_precedence)
- conflict niwa-no-git-amend-after-qa-submit <-> niwa-scribe-commit-is-head-after-submit-never-amend
- conflict DEC-general-3 <-> DEC-general-4
- conflict DEC-wraith-session-context-r2-1 <-> DEC-relay-session-context-1
Other hits, which agents will resolve (§1 predicted about 50% same-fact):
- DEC-general-1<->3
- wraith/yoru/trovex presubmit x2
- DEC-gate-integration-1<->2
- DEC-gate-escalation-1<->2
- wraith-lane-relay-project-is-tsukumo <-> niwa-agent-project-env-is-spawn-namespace
- niwa-reviewer-cannot-see-niwa-decision-md <-> niwa-ac-evidence-goes-in-commit-body
Found by the replay and fixed: prod has decision rows whose value is not JSON, so json_extract errored. The area query now guards it with json_valid.

TESTS: TestContradictions/{PG151617NoConflict, SameRankSameLayerDisagreeIsCandidate, DisjointValidityIsAmends, BehaviorContextNeverConflict, KeyFamilyAloneNotSubject, DeclaredSubjectMatches, DuplicateGroupOnce (re-run + 4 concurrent ticks + backfill re-run => 1 row), WriteTimeHintNoWrite (writer total_changes unchanged), ClaimLeaseCAS (3 claimers, 1 wins), TwoFailedClaimsEscalateToException, ReplayProd (skips without a backup; RELAY_REPLAY_DB overrides)}. Mutation-checked: area "general" exclusion off => KeyFamily fails; G1 top-K+Jaccard off => KeyFamily fails.

REJECTED: pairwise conflict rows (the design wants one row per group; coveredTx plus members_hash give insert-once). Area as a signal including "general" (it makes every DEC-general pair a candidate). Raw-JSON tokenization for decisions (noise).

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/{contradictions.go,contradictions_test.go,db.go}, internal/relay/{handlers_memory.go,cleanup.go}
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK / go test -tags fts5 ./... OK / TestToolSchemaBudget unchanged 54483

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- contradiction_mode / contradiction_overlap_min / conflict_lease_ttl are read via GetSetting but not declared in settingSpecs, so the REST settings PUT will refuse them (same follow-up shape as 6aa2179 for coherence). settingSpecs was not in the file list.
- The D1 hint has no handler-level test; the relay test file was not in the file list. The DB-level OverlapHint test covers overlaps, the suggestion and zero writes.
- The first tick after deploy runs the backfill: about 400 RO FTS queries and ~12 writer txs (1.5 s end to end on the prod copy, migration included).

## 3. Files changed

```
internal/db/contradictions.go      | 1021 ++++++++++++++++++++++++++++++++++++
 internal/db/contradictions_test.go |  426 +++++++++++++++
 internal/db/db.go                  |    5 +
 internal/relay/cleanup.go          |   20 +
 internal/relay/handlers_memory.go  |   30 +-
 5 files changed, 1501 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `461ec7c6-b30f-493d-ae0a-c232d19c8b82`._
