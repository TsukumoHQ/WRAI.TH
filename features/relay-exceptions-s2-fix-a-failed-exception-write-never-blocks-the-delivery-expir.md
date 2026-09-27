# [relay/exceptions] S2 fix: a failed exception write never blocks the delivery expiry or lease release it annotates

## Team : wraith-engine (tsukumo)
## Branch : fix/exception-write-best-effort (from main)
## Relay task : c6f6c5e3-8d37-4b6a-9e39-b9d5b8908515
## Trace : trace=e37d8bff657f5217238b0ee827b43524
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. with exception inserts forced to fail, ExpireDeliveries still moves the expiring deliveries to state expired, writes their deadletter journal rows, and returns the expired count; 0 exception rows exist for them (test)
- [ ] 2. with exception inserts forced to fail, a dead holder's expired lease is still requeued to pending with lease_holder NULL by the sweep, and a deactivated agent's lease is still released by the cascade (two tests)
- [ ] 3. with exception inserts working, the rows written by all three sites are unchanged: existing TestExceptions and task_sweeper tests pass unmodified
- [ ] 4. go test -tags fts5 ./internal/db/ green; go vet -tags fts5 ./... clean

## 2. Root cause & decisions

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/{exceptions.go (+bestEffortExceptionTx), deliveries.go, task_lease.go, referential_cascade.go, exceptions_test.go (+3 tests), referential_cascade_test.go (TestCascadeReleaseTxAtomic inverted)}. No schema, no MCP tool, no handler change.
Gate: build -tags fts5 OK / vet -tags fts5 ./... OK / gofmt OK / go test -tags fts5 ./... OK (all packages); go test -tags fts5 ./internal/db/ -run ExceptionWriteFailure: 3/3.

AC by test:
- AC1 expiry: TestExceptionWriteFailure_ExpireDeliveriesStillExpires (2 deliveries expired, 2 deadletter rows, returns 2, 0 exception rows, exception_classes unchanged).
- AC2 lease: TestExceptionWriteFailure_LeaseSweepStillRequeues (sweep) + TestExceptionWriteFailure_CascadeStillReleases (cascade); both pending/lease_holder NULL, 0 exception rows.
- AC3 success path unchanged: TestExceptions + task_sweeper tests pass unmodified.
- AC4 gate above.

BEHAVIOUR FLIP (not bar-weakening, flagged to wraith-cto 6885f74a): referential_cascade_test.go TestCascadeReleaseTxAtomic pinned the OLD contract (a failed exception write rolls the release back). AC2 mandates the opposite, so its asserts are inverted in place (Released=1, pending, holder NULL) and its comment cites c6f6c5e3; the test still forces the failure (drops the exceptions table), so it now also covers the "table gone" failure mode.

Checklist: §2 single writer: the savepoint runs on the same writerTx, no second writer, no new tx. CAS on (id, project, lease_holder, status) still decides before the exception write; a lost race still returns before it (writes nothing). §3 no schema/migration. Failure logged once per site call: `exceptions: write failed, primary write kept: source_kind=<k> source_ref=<ref>: <err>` (deadletter ref = expire-sweep@<now>; lease refs = task id).

BLOCKERS: none
NITS:
- An error that SQLite answers by rolling back the whole tx (SQLITE_FULL/IOERR) makes ROLLBACK TO fail; the helper returns that error and the primary aborts as before. Correct (the writer is broken), just not "best effort" in that case.

ROOT_CAUSE: the three exception writes shared the primary writer tx and their error returned/aborted before commit (deliveries.go ExpireDeliveries: returned before the expiry UPDATE; task_lease.go requeueExpiredLease and referential_cascade.go releaseDeactivatedLease: returned false, deferred Rollback undid the requeue).
DECISION: bestEffortExceptionTx(tx, sourceKind, sourceRef, fn) in exceptions.go, the guards_ladder.go:132-150 SAVEPOINT / ROLLBACK TO / RELEASE shape, wrapped around each of the three exception writes. On success the same statements run in the same order on the same tx, so rows are identical.
REJECTED: moving the exception write to its own tx after commit (a second write tx per sweep row, and a crash between them loses the journal entry silently); a prod failure-injection seam (ticket: test-only trigger).

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/db/ -run ExceptionWriteFailure -count=1
  test_sha: 09028fa
  output: |
    --- FAIL: TestExceptionWriteFailure_ExpireDeliveriesStillExpires (0.03s)
        exceptions_test.go:748: expire = 0, deadletter exceptions: insert exception: injected; want 2, nil
    --- FAIL: TestExceptionWriteFailure_LeaseSweepStillRequeues (0.03s)
        exceptions_test.go:777: sweep: <nil>, 0 swept; want 1
    --- FAIL: TestExceptionWriteFailure_CascadeStillReleases (0.03s)
        exceptions_test.go:804: cascade: <nil>, released=0; want 1
    FAIL	agent-relay/internal/db	0.434s

## 3. Files changed

```
internal/db/deliveries.go               |  4 +-
 internal/db/exceptions.go               | 19 +++++++
 internal/db/exceptions_test.go          | 92 +++++++++++++++++++++++++++++++++
 internal/db/referential_cascade.go      |  6 ++-
 internal/db/referential_cascade_test.go | 18 +++----
 internal/db/task_lease.go               |  6 ++-
 6 files changed, 133 insertions(+), 12 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c6f6c5e3-8d37-4b6a-9e39-b9d5b8908515`._
