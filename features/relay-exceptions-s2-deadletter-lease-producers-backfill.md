# [relay/exceptions] S2: deadletter + lease producers, backfill

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/exceptions-s2 (from main)
## Relay task : 3468f860-67cb-4184-9553-e44603e6ec88
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 deadletter: a broadcast to 3 live + 2 deleted recipients expiring -> exactly 2 exception rows, evidence live=3 gone=2; the deadletter table content is byte-identical with and without the exception write. Tests TestExceptions/DeadletterTwoGroupsMax, /DeadletterRowsUnchanged.
- [ ] 2. AC2 lease: expired lease of a dead holder -> one row resolved_by=self, resolution_reason=requeued; live holder -> none. Test /LeaseSweepOpensResolved.
- [ ] 3. AC3 backfill: on a fixture of 10 tasks + 20 deadletter rows, class and row counts match a checked-in expectation; second boot writes 0; marker set. Tests /BackfillCounts, /BackfillIdempotent. PR body: backfill row + class counts on a read-only COPY of a prod backup (design estimate 1943 rows, ~275 classes).

## 2. Root cause & decisions

ROOT_CAUSE: exceptions S1 recorded only task blocks and cancels. Expired unread deliveries (3245 deadletter rows) and lease-sweep requeues left no typed exception, so exception counts missed the delivery and lease paths, and the prod history was not in the table at all.

DECISION:
- P7 in ExpireDeliveries' existing tx. It reads the exact expiring set before the UPDATE, one group per message, and writes <=2 rows per message: recipient state live (agent active/sleeping, the same test as agentLive) or gone. The classes are structural, shape x state x priority (ruling OQ7), with no text key: keying on the subject gives ~735 singletons. source_ref = "<message_id>:<state>", so the two rows of one message stay distinct under UNIQUE(source_kind, source_ref, opened_at).
- P5: the sweep's CAS UPDATE and the exception share one beginWriterTx. RowsAffected==0 writes nothing. The best-effort audit stays after commit, as before.
- Backfill v1 runs behind a settings marker, in chunks of 500 per tx, with skip-if-exists (a boot that dies mid-way resumes with no duplicates). Historical resolvers are 'unknown' (never invented). A blocked task is always a block row, so the slice-1 unblock path resolves it later.

MEASURED (read-only COPY of relay.20260925T225329Z.pre-v1.22.0.db, scratch dir, never the live DB):
- 1943 exception rows (the design estimated 1943), 273 classes (the design estimated ~275).
- By source_kind (rows / classes / open): task_cancel 290/202/0, task_block 80/50/5, limbo_sweep 15/2/15, deadletter 1499/18/0, lease_sweep 54/1/0, agent_cascade 5/1/0. The 20 open rows are the 20 blocked tasks.

REJECTED:
- One row per message with both counts: it hides live failures inside routing noise.
- A trigger on deliveries: it can't classify, and the design invariant is the same tx.

[LEGACY_OPPORTUNITY] P5b cascade producer (referential_cascade.go:76) is a follow-up; the backfill already covers its 5 historical rows as agent_cascade.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/{exceptions.go,exceptions_test.go,deliveries.go,task_lease.go}
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 -race OK (1105 passed, 12 packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/deliveries.go ExpireDeliveries: a failing exception write now aborts the whole expiry tx (the design's same-tx invariant). The next sweep retries, but a persistent classifier error would stall expiry. Covered by tests; flagging it for awareness.
- deadletterExceptionsEnabled is a package var toggled only by DeadletterRowsUnchanged (no t.Parallel there; the -race run is clean).
- Single writer kept: P7 rides the existing tx, P5 swaps writerExec for beginWriterTx on the same statement, and the backfill runs in migrate on the writer conn. Migration is additive. No new hot-path write: P7 queries run only when deliveries expire.

REBASE r2 (onto 3027b1e class budgets T1): the conflict was in migrateExceptions; both calls are kept, migrateClassBudgets first, then backfillExceptionsV1. budget_epoch is stamped before the backfill, so every backfilled row predates it and history never counts toward a class budget. Verified on a fresh prod copy: 1943 rows, 273 classes, 0 rows with opened_at >= budget_epoch. Gate: 1119 tests passed, -race.

## 3. Files changed

```
features/DEBT.md                                   |   1 +
 ...tions-s2-deadletter-lease-producers-backfill.md |  70 ++++
 internal/db/deliveries.go                          |  56 +++
 internal/db/exceptions.go                          | 425 ++++++++++++++++++++-
 internal/db/exceptions_test.go                     | 308 +++++++++++++++
 internal/db/task_lease.go                          |  50 ++-
 6 files changed, 889 insertions(+), 21 deletions(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-3468f860-67cb-4184-9553-e44603e6ec88
- 🟢 AC1: test asserts len(got)==2, evidence live=3 gone=2 recipients=5, codes broadcast_unread:gone/live, re-run idempotent, second broadcast reuses class; row-unchanged test toggles gate and compares dl.to_agent||from||priority||subject||project concat — both pass — evidence: deliveries.go:447-454 inserts expireDeliveryExceptionsTx call behind deadletterExceptionsEnabled gate; exceptions.go:666-717 writeDeadletterExceptionsTx splits live/gone by deadletterLiveSQL count — test: TestExceptions/DeadletterTwoGroupsMax exceptions_test.go:393 + TestExceptions/DeadletterRowsUnchanged exceptions_test.go:447 (byte-identical deadletter output with/without the write)
- 🟢 AC2: test deactivates dead-holder, force-expires both leases, sweeps; asserts 1 exception for dead-holder with source=lease_sweep kind=lease_expired resolved_by=self reason=requeued raised_by=relay-sweeper, 0 for live-holder; CAS RowsAffected==0 path returns false without writing — pinned — evidence: task_lease.go:243-256 live holder continues before requeueExpiredLease; exceptions.go:723-738 leaseExceptionOpen builds Resolved{By:self, Reason:requeued} — test: TestExceptions/LeaseSweepOpensResolved exceptions_test.go:495
- 🟢 AC3: BackfillCounts asserts n==23 rows and per source_kind {task_block:3/3, limbo_sweep:2/1, task_cancel:4/3, deadletter:12/9, lease_sweep:1/1, agent_cascade:1/1}, classes=18, open=2 (blocked); BackfillIdempotent asserts marker=done after first run, second boot n==0, marker-lost skip-if-exists n==0; both pass — evidence: exceptions.go:743-773 backfillExceptionsV1 behind settings key backfill_exceptions_v1, marker set only after all 3 steps succeed; 820/899/933 per-source backfills; PR commit body states Prod backup copy: 1943 rows, 273 classes — test: TestExceptions/BackfillCounts exceptions_test.go:522 + TestExceptions/BackfillIdempotent exceptions_test.go:566

## 5. Timeline

- round 1 → **approve** (review-3468f860-67cb-4184-9553-e44603e6ec88)

**Approve-with-findings (follow-up):** go test -tags fts5 -race ./internal/db/... ./internal/relay/... 1022 passed; all 3 ACs verified by named behavioral tests; AC1 2 exception rows live=3 gone=2 (DeadletterTwoGroupsMax + DeadletterRowsUnchanged), AC2 1 row resolved_by=self requeued for dead holder, 0 for live (LeaseSweepOpensResolved), AC3 23 rows in 18 classes + marker set + idempotent (BackfillCounts + BackfillIdempotent)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `3468f860-67cb-4184-9553-e44603e6ec88`._
