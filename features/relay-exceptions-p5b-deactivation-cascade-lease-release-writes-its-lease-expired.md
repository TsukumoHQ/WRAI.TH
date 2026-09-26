# [relay/exceptions] P5b: deactivation-cascade lease release writes its lease_expired exception in the same tx

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/exceptions-p5b (from main)
## Relay task : ff31c3ab-7eaa-4f6b-8552-6f154ee99952
## Trace : trace=a065ddd2c01156f39771c070bfcb9b8f
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 cascade of a deactivated holder with one leased task: task pending and exactly one exceptions row for it with code agent_deactivated, kind lease_expired, resolved_by self, resolution_reason requeued (test TestCascadeReleaseOpensResolvedException).
- [ ] 2. AC2 lost CAS race (task claimed live before the release): 0 exception rows and the task unchanged (TestCascadeSkipsLiveRacedClaim extended with the row count).
- [ ] 3. AC3 forced exception INSERT failure: task status and lease_holder unchanged and the task absent from AgentCascade.Released (test TestCascadeReleaseTxAtomic, pattern of the exceptions TxAtomic test).
- [ ] 4. AC4 existing internal/db cascade and exceptions tests pass unchanged; no schema or tool-schema diff.
- [ ] 5. AC5 go vet ./... clean; go test -tags fts5 -race ./internal/db/... green.

## 2. Root cause & decisions

# [relay/exceptions] P5b: deactivation-cascade lease release writes its lease_expired exception in the same tx

Task: ff31c3ab-7eaa-4f6b-8552-6f154ee99952

ROOT_CAUSE: `CascadeAgentDeactivation` (internal/db/referential_cascade.go) released a deactivated holder's leased tasks with an autocommit `writerExec` and wrote no exception. Live deactivation requeues were invisible to the exceptions ladder; only the backfill had recorded that history.

DECISION (design 220f4f3d producer row P5b, same shape as P5 `requeueExpiredLease`):
- New `releaseDeactivatedLease` runs the release in one writer tx: `beginWriterTx`, the CAS UPDATE, then `RowsAffected==0` returns false and writes nothing, then `openExceptionTx(tx, leaseExceptionOpen(..., "agent-deactivated", ...))`, then Commit. The CAS SQL is byte-identical to main.
- The cascade SELECT also reads `trace_id` (the same column `SweepExpiredLeases` reads), so the exception carries the task's trace.
- `auditLeaseTransfer` and `out.Released` append only after a successful Commit. A failed exception write rolls back the release, and the task stays leased; the lease sweep remains the backstop.
- Step 2 (MarkQuarantine limbo) and step 3 (membership soft-close) are untouched. No schema change and no tool-schema change.
- exceptions.go: comment only (P5b is no longer backfill-only).
- One tx per released task. The cascade runs on deactivation, which is not a hot path.

FILES: internal/db/referential_cascade.go, internal/db/referential_cascade_test.go, internal/db/exceptions.go (comment)

TESTS:
- AC1 `TestCascadeReleaseOpensResolvedException`: one row, code `agent_deactivated`, kind `lease_expired`, source agent cascade, resolved_by `self`, resolution_reason `requeued`.
- AC2 `TestCascadeSkipsLiveRacedClaim` extended: it now calls `releaseDeactivatedLease` directly with the stale holder. The result is a lost CAS, the task is unchanged, and it has 0 exception rows. The old test never reached the CAS because the list query already filters on holder.
- AC3 `TestCascadeReleaseTxAtomic`: with the exceptions table dropped, the task stays in-progress under holder and is absent from Released.
- AC4: existing cascade and exceptions tests pass unchanged.

## review-wraith verdict: SHIP
Scope: internal/db/referential_cascade.go, internal/db/referential_cascade_test.go, internal/db/exceptions.go (comment)
Gate: build -tags fts5 OK / vet -tags fts5 ./... OK / gofmt OK / test -count=1 -tags fts5 -race ./internal/db/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- exceptions.go:755 `leaseExceptionOpen` stamps raised_by `relay-sweeper` for the cascade too, while the audit trail says `relay-cascade`. This is pre-existing, and the backfill already writes it, so I left it unchanged to keep fingerprints and the backfill consistent.

RED_EVIDENCE:
  cmd: go test -count=1 -tags fts5 -race ./internal/db/...   (origin/main 79325d8 code + referential_cascade_test.go from 61a6c55)
  test_sha: e13d371
  output: |
    # agent-relay/internal/db [agent-relay/internal/db.test]
    internal/db/referential_cascade_test.go:160:7: d.releaseDeactivatedLease undefined (type *DB has no field or method releaseDeactivatedLease)
    FAIL	agent-relay/internal/db [build failed]
    FAIL

## Round 1 response (gate verify exit 1, reviewer approve, zero findings)
NOISE (non-reproducible): the gate verify tail holds only migration log lines; no FAIL line was captured. Reviewer: 3/3 runs exit 0. Author: `go test -tags fts5 -race -count=5 ./internal/db/...` exit 0, 0 `--- FAIL`, ok 213.6s, on 61a6c55 at the same base 79325d8. The run landed while 6 gate reviews were in flight (fleet cap 3 exceeded), so load is the likely cause. Resubmitted unchanged.

## 3. Files changed

```
...scade-lease-release-writes-its-lease-expired.md | 85 ++++++++++++++++++++++
 internal/db/exceptions.go                          |  4 +-
 internal/db/referential_cascade.go                 | 51 +++++++++----
 internal/db/referential_cascade_test.go            | 83 +++++++++++++++++++++
 4 files changed, 207 insertions(+), 16 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-ff31c3ab-7eaa-4f6b-8552-6f154ee99952
- 🟢 AC1: Implementation writes the agent_deactivated lease_expired exception opened+resolved atomically with the CAS release; behavior verified by the named behavioural test. — evidence: internal/db/referential_cascade.go:155-177 releaseDeactivatedLease opens one writer tx, runs the UPDATE+CAS, then openExceptionTx with leaseExceptionOpen(..., "agent-deactivated", ...) which the helper at internal/db/exceptions.go:745-760 maps to source=excSourceAgentCascade, code="agent_deactivated", kind="lease_expired", Resolved.By="self", Resolved.Reason="requeued". Verified by behaviour via `go test -tags fts5 -race -run TestCascadeReleaseOpensResolvedException ./internal/db/` → PASS. — test: TestCascadeReleaseOpensResolvedException (internal/db/referential_cascade_test.go:174-204) asserts taskStatus=="pending", len(res.Released)==1, excRowsFor==1, code/kind/source/resolved_by/reason all match.
- 🟢 AC2: CAS guard prevents both task mutation AND exception write on a lost race — verified. — evidence: internal/db/referential_cascade.go:165-168: on a stale holder, RowsAffected()==0 short-circuits before openExceptionTx. `go test -tags fts5 -race -run TestCascadeSkipsLiveRacedClaim ./internal/db/` PASS includes the new assertions at lines 159-168 (releaseDeactivatedLease returns false, task status/holder unchanged, excRowsFor==0). — test: TestCascadeSkipsLiveRacedClaim extension (internal/db/referential_cascade_test.go:159-168) calls releaseDeactivatedLease directly with the stale holder and asserts the row count is 0.
- 🟢 AC3: Atomicity verified by forced-INSERT-failure pattern matching exceptions TxAtomic style. — evidence: releaseDeactivatedLease opens one tx and defers tx.Rollback (internal/db/referential_cascade.go:157-158, 177). On an openExceptionTx error the function returns false so the audit lease_transfer is also skipped (lines 78-79). TestCascadeReleaseTxAtomic drops exception_occurrences view + exceptions table before the cascade; on INSERT failure the tx rolls back. `go test -tags fts5 -race -run TestCascadeReleaseTxAtomic ./internal/db/` → PASS. — test: TestCascadeReleaseTxAtomic (internal/db/referential_cascade_test.go:209-241) drops the table then asserts len(res.Released)==0, status=="in-progress", lease_holder=="holder".
- 🟢 AC4: No schema or tool-schema diff; existing internal/db tests green unchanged. — evidence: git show -s --stat on 61a6c55 lists only the 3 Go files (no .sql / migrations / schema / tool-schema files). Full suite `go test -tags fts5 -race ./internal/db/` ran 3× (count=1 + count=2) and produced exit=0 with ok on agent-relay/internal/db; TestCascadeReleasesLeasedMarksAssignedClosesMemberships, TestCascadeSkipsLiveRacedClaim base, all exceptions tests, and the rest of the 535-test suite passed unchanged. — test: All pre-existing tests pass at the same commit (no test deleted, ignored or emptied in this diff).
- 🟢 AC5: Hard gate satisfied on re-execution; the original failure does not reproduce. — evidence: `go vet ./...` → No issues found. `go test -tags fts5 -race ./internal/db/...` → EXIT=0, ok agent-relay/internal/db 26.587s (and again at 70.812s on -count=2). Note: dispatcher verify claimed exit=1 once; my reproduction (3× consecutive runs, fresh cache) is consistently exit=0 — likely transient on the dispatcher side. — test: verify_cmd `go test -tags fts5 -race ./internal/db/...` (meta.json verify.cmd).

## 5. Timeline

- round 1 → **reject** (review-ff31c3ab-7eaa-4f6b-8552-6f154ee99952)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ff31c3ab-7eaa-4f6b-8552-6f154ee99952`._
