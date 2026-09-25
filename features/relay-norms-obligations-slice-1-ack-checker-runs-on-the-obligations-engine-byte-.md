# [relay/norms] obligations slice 1: ACK checker runs on the obligations engine, byte-for-byte

## Team : wraith-engine (tsukumo)
## Branch : wraith/obligations-s1 (from main)
## Relay task : f77efe18-6504-4d92-9c98-fd69ef1d66bd
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. norms and obligations tables exist per design trovex 84f68979 (closed Go enums for trigger/what/while/bearer; UNIQUE(norm_id, bindings_hash) with INSERT OR IGNORE); a DB without them gains them and a second migrate is a no-op.
- [ ] 2. Each obligation transition is one writer transaction that CASes obligations.state and the legacy tasks.ack_*_at guard together; at most one sanction per subject per tick (tests TransitionCAS, OneSanctionPerTick).
- [ ] 3. TestACKEquivalence runs the legacy checkUnackedTasks (kept verbatim) and the obligations path on twin DBs over the same scenarios and gets identical messages, marks and timestamps, quirks Q1-Q4 included.
- [ ] 4. The equivalence test is part of the default go test run (no build tag, no skip) so every PR touching obligations runs it in CI.
- [ ] 5. go vet clean; go test -tags fts5 -race ./internal/... green.

## 2. Root cause & decisions

# f77efe18 — obligations engine slice 1: ACK checker on norms/obligations, byte-for-byte

ROOT_CAUSE: the ACK checker was a hard-coded if/else-if over tasks.ack_*_at; the norms it enforces (notify at 15m, escalate at 45m) had no data model, so no norm could be added, inspected or measured (late-done thrown away). DEC-wraith-obligations-1 rules slice 1: same behaviour, new engine, proven against the legacy checker.

DECISION (design 50b4b349 / trovex 84f68979 §3-§6):
- db.go: norms + obligations tables and 3 indexes (IF NOT EXISTS), UNIQUE(norm_id, bindings_hash); INSERT OR IGNORE seed of ack.escalate (eval_order 1) and ack.notify (2) with the exact legacy texts, settings keys, defaults and 1m..24h clamps.
- obligations.go: closed Go enum consts; InstantiateTaskAck (opens rows for tasks entering the legacy candidate set, pre-closed unfulfilled when the legacy mark is already set, so the 1090/813 prod marks never re-fire); ActiveTaskObligations (RO, candidate predicate = GetUnackedTasks'); TransitionTaskAck (one writer tx: obligation CAS active->unfulfilled AND the verbatim legacy tasks guard, whitelisted column; either 0 rows -> rollback, ok=false); CloseMootTaskObligations (fulfilled beats inactive; no notice); StampLateDone.
- cleanup.go: checkUnackedTasks -> evaluateObligations(db, notifier, now) on the unchanged StartACKChecker loop. Escalate weighed before notify, one sanction branch per task per tick even when the CAS refuses (legacy if/else-if: Q1, Q3). Push to dispatched_by after the mark (Q2, Q4). ackNotifier interface (*SessionRegistry satisfies it) is the seam for the recording notifier.
- The legacy checker lives on only as the oracle, verbatim in internal/relay/obligations_equivalence_test.go (renamed legacyCheckUnackedTasks, registry param typed ackNotifier).

Every write happens only when an obligation moves (NoWriteWhenNothingMoves: total_changes unchanged on an idle sweep).

## review-wraith verdict: SHIP
Scope: internal/db/{db.go,obligations.go,obligations_test.go}, internal/relay/{cleanup.go,obligations_equivalence_test.go}.
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/... green (2 runs).

BLOCKERS: none.
- Single writer: every write is a beginWriterTx on the existing 5-min tick; reads on the RO pool; no hot-path write.
- CAS: transitions are guarded conditional UPDATEs + RowsAffected, never SELECT-then-UPDATE (TransitionCAS, CASRaceOneWinner 8 goroutines -> 1 winner).
- Additive schema; old binary ignores the tables; migrate idempotent (SchemaAndOldDBMigrates).
- Equivalence: TestACKEquivalence 15 scenarios (fresh, 15-45m, >45m first sight Q1, notify->escalate, claimed/cancelled/archived/run-container between ticks, settings changed between ticks, notify_age >= escalate_age Q1, unparseable dispatched_at, pre-existing marks, several tasks in one tick). Mutation check: dropping the one-sanction-per-tick guard fails the two Q1 scenarios.

ROUND 2 (review r1 fixes):
- Closed enums: WhatTaskLeftPending / WhileTaskPendingLive / BearerAssigneeProfile consts; the engine only runs norms matching all four enums (TestObligations/NormEnumsClosed).
- Exact timestamps: DB.Now clock read by the legacy marks and the checker loop; TestACKEquivalence pins one instant per tick and compares ack_*_at values byte-for-byte (1us mutation fails 10/15 scenarios; TestObligations/ClockPinsLegacyMarks).

NITS (non-blocking):
- Notices are compared per tick as a sorted set: the legacy candidate read has no ORDER BY, so only the per-tick set is defined (at most one notice per task per tick).
- DB.SetClock is a test seam (production leaves it nil = time.Now).
- The legacy log lines keep their text except the mark-error line ("ACK <norm> mark error").

## 3. Files changed

```
...checker-runs-on-the-obligations-engine-byte-.md |  65 ++++
 internal/db/db.go                                  |  79 +++++
 internal/db/obligations.go                         | 325 ++++++++++++++++++++
 internal/db/obligations_test.go                    | 339 +++++++++++++++++++++
 internal/db/tasks.go                               |   4 +-
 internal/relay/cleanup.go                          | 105 ++++---
 internal/relay/obligations_equivalence_test.go     | 300 ++++++++++++++++++
 7 files changed, 1177 insertions(+), 40 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-f77efe18-6504-4d92-9c98-fd69ef1d66bd
- 🔴 AC1: [partial] schema+migration verified; what/while_pred missing Go-level closure but unused in dynamic SQL — evidence: internal/db/db.go:732-790 creates norms+obligations tables; UNIQUE(norm_id,bindings_hash) at line 775; INSERT OR IGNORE seeded norms. SchemaAndOldDBMigrates test drops tables and re-migrates twice. BUT closed Go enums in obligations.go:27-31 only define SubjectTask, TriggerTaskPendingUnclaim, BearerAssigneeProfile — no Go enum for what/while_pred columns (stored as TEXT only, never validated at Go level). — test: TestObligations/SchemaAndOldDBMigrates internal/db/obligations_test.go:56
- 🟢 AC2: CAS atomic, race verified (1 winner out of 8 goroutines), per-tick sanction cap verified — evidence: internal/db/obligations.go:173-204 TransitionTaskAck uses single beginWriterTx with both UPDATE obligations and UPDATE tasks in one tx; deferred Rollback. evaluateObligations cleanup.go:373 uses sanctioned[o.TaskID] map to enforce at most one sanction per task per tick. — test: TestObligations/TransitionCAS internal/db/obligations_test.go:117 + TestObligations/TransitionCAS/CASRaceOneWinner:158 + TestObligationsOneSanctionPerTick internal/relay/obligations_equivalence_test.go:279
- 🔴 AC3: [partial] behavioral+text equivalence verified; exact timestamp values not pinned in comparison — evidence: TestACKEquivalence obligations_equivalence_test.go:180-260 runs legacy oracle (kept verbatim: diff /tmp/main_cleanup.go:335-395 vs /tmp/equiv_test.go:29-82 byte-identical) against evaluateObligations on twin DBs across 15 scenarios including Q1 (NotifyAgeAtOrAboveEscalateAgeQ1), Q2 (SeveralTasksOneTick), Q3 (OneSanctionPerTick), and unparseable/preexisting marks. NOTICE STRINGS + mark NULLNESS compared; exact timestamps (ack_notified_at/ack_escalated_at values) NOT compared (both use time.Now().UTC but at different call sites, so microseconds differ). — test: TestACKEquivalence internal/relay/obligations_equivalence_test.go:180 (15 scenarios all PASS)
- 🟢 AC4: default go test, no tags, no skip — every PR touching obligations runs it — evidence: obligations_equivalence_test.go has package relay, no build tags, no t.Skip; TestACKEquivalence + TestACKEquivalenceCoversBothSanctions + TestObligationsOneSanctionPerTick all run in default go test ./internal/relay/. Verified: go test -tags fts5 -race ./internal/relay/ PASS (17 subtests in TestACKEquivalence). — test: TestACKEquivalence + TestACKEquivalenceCoversBothSanctions + TestObligationsOneSanctionPerTick
- 🟢 AC5: CI gate green — evidence: go vet -tags fts5 ./internal/... → No issues found. go test -tags fts5 -race ./internal/... → all 993 tests pass across 11 packages in 39s. — test: go vet clean; full ./internal/... suite green

## 5. Timeline

- round 1 → **reject** (review-f77efe18-6504-4d92-9c98-fd69ef1d66bd)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `f77efe18-6504-4d92-9c98-fd69ef1d66bd`._
