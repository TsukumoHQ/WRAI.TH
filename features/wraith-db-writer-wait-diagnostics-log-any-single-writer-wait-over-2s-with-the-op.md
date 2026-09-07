# [wraith/db] writer-wait diagnostics: log any single-writer wait over 2s with the op — writer starvation is invisible today

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/db-writer-wait-diag (from main)
## Relay task : 2d5fb3c2-3f9a-4368-903f-d1ed8b1de6c9
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: with writerSlowWait shrunk to 20ms and the writer held by an open tx for 60ms, a concurrent writerExec logs exactly one line containing `writer wait:` and the query prefix; revert-check: threshold raised above the hold -> no line
- [ ] 2. AC2 named test: a writerExec that completes below writerSlowWait logs nothing (buffer empty)
- [ ] 3. AC3 named test: beginWriterTx waiting behind a held writer logs one `writer wait: begin-tx caller=` line naming the calling function
- [ ] 4. AC4 in-diff: writerExec error path (deadline exceeded) still returns the error unchanged AND logs the wait line — named test asserts both
- [ ] 5. AC5 scope: writerTimeout and referentialScanTimeout values byte-identical; existing 13 tests in referential_integrity_test.go and wedge_test.go green; diff touches at most 3 files

## 2. Root cause & decisions

ROOT_CAUSE: Writer starvation was invisible — a scan/flush holding the sole writer connection (SetMaxOpenConns(1)) made every other write silently wait out writerTimeout, so the 91 `context deadline exceeded` lines since 2026-09-02 had no attributable cause in the log. Fix: log any single-writer acquire/exec that crosses writerSlowWait (2s), and the referential scan's total duration, with the op — observability only, zero change to writerTimeout, the pool, DSN, or any SQL.

## review-agent-runtime verdict: SHIP

```
review-agent-runtime: PASS
Scope: internal/db/db.go (+51/-2), internal/db/referential_integrity.go (+10), internal/db/db_writerwait_test.go (+190)
BLOCKERS: none
  §1 schema/scan: untouched — no agentColumns/scanAgent edit, no migration.
  §2 concurrency: writerExec/beginWriterTx logic unchanged; added only start:=time.Now()+conditional log.Printf (stdlib log is mutex-guarded); refScanReadHook is a test-only seam, nil in prod. No new shared mutable state.
  §3 single-writer: observability-only — no new writer handle, no DSN change, no new per-request SQLite write. The diagnostic is log.Printf, bounded by the writerSlowWait=2s threshold (low volume).
  §4 boundary / §5 MCP surface / §6 lifecycle: N/A — diff touches none.
  AC5: writerTimeout / referentialScanTimeout assignments byte-identical to main (git diff shows no +/- on those lines).
NITS: none.
```

Tests: 6 AC tests in db_writerwait_test.go — writerExec slow-wait logs one prefixed line (AC1) / high-threshold reverts to silent (AC1) / uncontended is silent (AC2), begin-tx logs caller via runtime.Caller(1) (AC3), error/deadline path still logs + returns err (AC4), scan logs `integrity scan: took` on slow duration. `go test -tags fts5 ./internal/db/...` 313 pass; `-race -count=3` on the 6 new = 18 pass, no data race; `go vet` clean; gofmt clean.

## 3. Files changed

```
internal/db/db.go                    |  51 +++++++++-
 internal/db/db_writerwait_test.go    | 190 +++++++++++++++++++++++++++++++++++
 internal/db/referential_integrity.go |  10 ++
 3 files changed, 249 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `2d5fb3c2-3f9a-4368-903f-d1ed8b1de6c9`._
