# [relay/exceptions] S1: exceptions + classes tables, deterministic classifier, task-path producers

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/exceptions-s1 (from main)
## Relay task : 7b2392c4-ae35-40ae-a4b2-aeb398f21652
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 block + re-block + resolve: BlockTask opens exactly one open row (source_ref=task, class resolved, occurrences=1); a second task blocked with the same text but different id/agent/time gets the same class_id (occurrences=2); unblock by blocker -> resolved_by=self, operator cancel -> human, other agent -> peer. Tests TestExceptions/BlockOpensOne, /ReblockSameTextSameClass, /UnblockResolvesWithDerivedResolver.
- [ ] 2. AC2 cancel classification: cancel with a lexicon-classified reason (e.g. superseded) -> one row opened+resolved, kind=plan_change, retry_class=benign; cancel with unclassifiable prose -> retry_class=unknown. Test /CancelWithReason (both cases).
- [ ] 3. AC3 atomicity + determinism + migration: two concurrent BlockTask on one task -> one transition, one exception (/CASLoserWritesNothing); forced exception INSERT failure -> task status unchanged (/TxAtomic); same input in two fresh DBs -> identical fingerprint and template (/NoModelCallDeterministic); taskColumns count == scanTask targets (/TaskColumnsUntouched); old DB gains tables, second migrate no-op (/OldDBMigrates).

## 2. Root cause & decisions

# 7b2392c4 — exceptions S1: tables, deterministic classifier, task-path producers

ROOT_CAUSE: blocked_reason is one prose column doing three jobs (block reason, cancel reason, sweep stamp), overwritten on every re-block/cancel/reset, with no class, count or resolution record, so recurring friction (dead lanes, misroutes, Niwa gate exhaustion) was only visible by grepping prose (design 220f4f3d §1).
DECISION (design designs/220f4f3d-exceptions.md §3/§4/§7 slice 1, ruling cto-tsukumo 10b62e06):
- internal/db/exceptions.go (new): migrateExceptions (exceptions, exception_classes, indexes, exception_occurrences view over integrity_quarantine); L0 lexicon v1 (17 codes, ordered, first match wins); parameterize (Sentry token list + hexid-must-mix-digit-and-letter + agent names read on the tx); fingerprint = sha1(kind|code|grouping_version=1|first-line tokens<=24)[:16]; classifyTx: exact class fingerprint -> exact variant fingerprint on an earlier occurrence -> Drain attach in the (kind, code) bucket (depth 3, sim >= 0.5, template generalized to <*>) -> new class. openExceptionTx / resolveOpenTx / excResolver (human | self | supervisor=dispatcher | peer, derived from who moved the task).
- internal/db/tasks.go transitionTask: blocked, cancelled-with-reason and any move out of blocked run the status CAS UPDATE and the exception write in ONE beginWriterTx; RowsAffected==0 -> TASK_STATE_CONFLICT, rollback, no exception. Commit happens before the existing last_activity_at / lease follow-ups (single writer conn: they must run after the tx releases it). Every other transition keeps the autocommit writerExec.
- internal/db/limbo_sweep.go blockLimboTask: same wrap, source-declared class limbo_sweep/limbo/retryable, raised_by relay-sweeper.
- internal/db/db.go: migrateExceptions(conn) right after integrity_quarantine (the view needs it).
RULING CONDITIONS: cancels are plan_change/benign only when the lexicon classifies them; unclassified text keeps retry_class=unknown (TestExceptionLexiconUnclassifiedNeverBenign, /CancelWithReason). Classes global (fingerprint has no project; exceptions.project keeps counts). grouping_version=1, no secondary-grouping writer (the secondary_fingerprint column exists per the design schema and is never written). No trigger. No model call. taskColumns/scanTask untouched.
DEVIATION from design §4, flagged: exceptions carries fingerprint, matched_by, match_distance per occurrence. Reason: a Drain-attached variant needs its own fingerprint persisted so the next identical text resolves by exact lookup and never re-matches (the persisted-decision rule in §3.1); without it the design's schema had nowhere to store the variant->class decision.
REJECTED: SQLite trigger (ruling OQ1: rows would land unclassified); a separate exception_fingerprints table (same information, one more table); caching the agent-name regex (premature: ~3 ms per block/cancel/unblock measured on the prod copy, see nits).

## Prod-copy dry run (DoD)
Go classifier (this branch) over a COPY of backup relay.20260925T225329Z.pre-v1.22.0.db (scratch copy, deleted after; live DB never opened): 385 blocked_reason rows -> 385 exceptions, **253 classes** (216 singletons); matched_by new 253 / exact 123 / drain 9.
Lexicon v1 codes (rows / classes / retry_class):
superseded 79/57 benign · unclassified 79/73 unknown · dead_lane 44/8 non_retryable · obsolete 28/16 benign · postponed 27/26 benign · operator_override 22/16 benign · limbo_sweep 15/2 retryable · stale_no_output 13/5 retryable · duplicate 12/12 benign · gate_pending 12/12 retryable · niwa_gate_rounds_exhausted 12/2 non_retryable · cancelled_by_request 11/11 benign · niwa_merge_blocked 10/5 retryable · deprioritized 9/2 benign · misrouted 9/3 non_retryable · done_elsewhere 2/2 benign · niwa_gate_postmerge_failed 1/1 non_retryable.
Matches the design's Python dry run (17 codes, 79.5% coverage, 251 templates).

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/exceptions.go (new), exceptions_test.go (new), tasks.go (transitionTask), limbo_sweep.go (blockLimboTask), db.go (one call). No handler/MCP/REST/updater change.
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / CGO_ENABLED=1 go test -tags fts5 -race ./internal/db/... OK (and -count=3 OK, no flakes) / ./internal/relay/... OK.
AC1: TestExceptions/BlockOpensOne, /ReblockSameTextSameClass (exact match, occurrences=2, template has no agent/task id/PR/timestamp), /UnblockResolvesWithDerivedResolver (self, human, peer, + supervisor), /DrainAttachPersistsVariant.
AC2: /CancelWithReason (superseded -> plan_change/benign resolved; French prose -> unclassified/unknown; no reason -> no row).
AC3: /CASLoserWritesNothing (2 goroutines -> 1 transition, 1 row), /TxAtomic (exceptions table dropped -> BlockTask errors, task still in-progress, 0 classes), /NoModelCallDeterministic (two fresh DBs -> same fingerprint + template), /TaskColumnsUntouched (scanTask over taskColumns), /OldDBMigrates (tables dropped, migrate x2, writes again). Extra: /QuarantineInView, /LimboBlockSourceDeclared (incl. raced no-op writes nothing, resume resolves).
Thesis checks: single writer kept (the exception write rides the existing writer tx; no new writer, no new goroutine); no read-path write; only block/cancel/unblock paths gain statements (claim/start/done untouched unless leaving blocked); migration additive (CREATE ... IF NOT EXISTS, view IF NOT EXISTS); agentColumns/taskColumns untouched.
BLOCKERS (must fix before merge):
- none
NITS (non-blocking):
- exceptions.go agentNameRE: the agent-name regex is compiled per classification while the writer is held (~3 ms on the 285-agent prod copy). Cache by roster hash if block volume grows.
- migrateExceptions follows the file's `_, _ = conn.Exec` pattern; a failed CREATE would surface later as block/cancel errors rather than at boot (same as every other table in migrate).

## 3. Files changed

```
internal/db/db.go              |   4 +
 internal/db/exceptions.go      | 567 +++++++++++++++++++++++++++++++++++++++++
 internal/db/exceptions_test.go | 412 ++++++++++++++++++++++++++++++
 internal/db/limbo_sweep.go     |  28 +-
 internal/db/tasks.go           |  54 +++-
 5 files changed, 1050 insertions(+), 15 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `7b2392c4-ae35-40ae-a4b2-aeb398f21652`._
