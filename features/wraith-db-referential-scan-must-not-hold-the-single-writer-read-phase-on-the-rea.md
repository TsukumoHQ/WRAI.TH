# [wraith/db] referential scan must not hold the single writer: read phase on the reader pool, one short apply tx by row_id lists

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/db-refscan-writer-split (from main)
## Relay task : 1ac2ce6e-40b6-41d8-a2ab-d282a14e267d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: while a scan's read phase is in progress (fixture with a slow orphanSQL or a hook that pauses between classes), a concurrent writerExec from another goroutine completes within 100ms — the scan does not hold the writer during reads; revert-check: running the read phase on d.conn inside a tx makes the concurrent write wait
- [ ] 2. AC2 named test: on the existing 13 referential_integrity_test.go fixtures the scan output is byte-identical to today's: per-class open counts, the detect/heal/reopen row sets, and the emitted log lines — including the orphan_profile pool-key OR-clause behavior (DEC-wraith-orphan-profile-burndown-1)
- [ ] 3. AC3 named test: a ref healed between the read phase and the apply tx is NOT resolved in that scan and IS resolved by the next scan (documents the accepted one-tick race)
- [ ] 4. AC4 in-diff: the apply tx contains no orphanSQL text — a named test or a grep-able assertion that the only statements executed on the writer are INSERT OR IGNORE / UPDATE ... WHERE row_id IN (...) against integrity_quarantine; the apply tx is opened via beginWriterTx (writerTimeout-bounded)
- [ ] 5. AC5 scope: diff touches at most internal/db/referential_integrity.go, internal/db/referential_integrity_test.go, internal/db/db.go; every orphanSQL string byte-identical (no LOWER() change); go test -tags fts5 ./internal/db/... green

## 2. Root cause & decisions

# Decision — referential scan off the single writer (task 1ac2ce6e)

ROOT_CAUSE: `runReferentialScan` ran all ~16 orphan classes' five statements — each embedding the class `orphanSQL` — inside ONE writer transaction on `d.conn` (the single writer, `SetMaxOpenConns(1)`), bounded by `referentialScanTimeout` (30s). That is > `writerTimeout` (15s), so under fleet load the 20–40s scan held the sole writer connection while every other write (send_message / claim_task / complete_task / token flush / expire) deadlined behind it — 91 `context deadline exceeded` lines since 09-02.

## Decision
Split the scan into a READ phase and an APPLY phase (the reader-scan-then-short-write shape already used by `dangling_board.go` and `limbo_sweep.go`):

- **READ** (reader pool `d.ro()`, own `referentialScanTimeout` ctx): per class run the `orphanSQL` ONCE, load the open/resolved quarantine sets, compute in Go `newOrphans = orphan\open`, `reopen = resolved∩orphan`, `resolve = open\orphan`. Holds no writer.
- **APPLY** (ONE `beginWriterTx`, `writerTimeout`-bounded): only `INSERT OR IGNORE` (explicit values) and `UPDATE … WHERE row_id IN (…)` against `integrity_quarantine`, IN-lists chunked at 500. No `orphanSQL` text runs on the writer, so it is held for the milliseconds the deltas need, not the length of the scan.

`readRefDeltas` / `applyRefDeltas` are shared by the live split path (`(*DB).RunReferentialScan`) and the boot-time single-tx path (`runReferentialScan(conn)`), so the SQL and the delta arithmetic are one source.

## Invariants held
- Every `orphanSQL` string byte-identical (incl. the DEC-wraith-orphan-profile-burndown-1 pool-key OR-clause). **No `LOWER()` change — that is ticket B, explicitly out of scope here.**
- Log lines (`integrity: detect …`, `integrity: heal …`) byte-identical, still emitted post-commit only.
- Single-writer discipline: the ONLY writer use in this file is the apply tx. No new goroutines, no schema change.

## Accepted race (ruled)
A ref that heals between the read phase and the apply tx is NOT resolved in that scan; the next scan (2 min later) resolves it. Idempotent. Covered by `TestScanHealBetweenReadAndApply` (AC3).

## Boot / reconcile path
`runReferentialScan(conn)` (migrate `db.go:1339`, reconcile `referential_reconcile.go:87`) runs before the reader pool exists and under no fleet contention, so it keeps the single-tx shape — now built on the same shared helpers. Kept in scope-safe: those callers were NOT edited. `TestBootScanMatchesSplit` proves boot and split return identical counts.

## Rejected alternatives
- **Keep one tx but shrink the timeout** — would just move the guillotine, not stop the writer being held for the whole scan.
- **Duplicate the whole scan for the live path** — rejected: two copies of the orphan detect/heal/apply orchestration would drift; instead the `orphanSQL` source (`refChecks`) and the delta/apply helpers are shared.

## Scope
`internal/db/referential_integrity.go` + `internal/db/referential_integrity_test.go` (2 of the 3 allowed files; `db.go` not needed).

## Verify
`go test -tags fts5 ./internal/db/...` green; full `go test -tags fts5 ./...` green; AC1/AC3/AC4 + parity tests green 3× under `-race`.

## review-agent-runtime verdict: SHIP

review-agent-runtime: PASS
Scope: internal/db/referential_integrity.go + internal/db/referential_integrity_test.go (referential scan; read/apply phase split)
BLOCKERS: none
- §1 schema/scan lockstep: N/A — no agentColumns/scanAgent/schema/migration change.
- §2 concurrency: apply uses guarded conditional UPDATEs (row_id IN explicit lists, resolved_at state guard) + idempotent INSERT OR IGNORE; single background sweeper (no concurrent scans); read↔apply gap is the ruled one-tick race (AC3). Explicit IN-lists are strictly safer than the old NOT IN (orphanSQL).
- §3 single-writer/availability: writer now held only for the short apply tx via beginWriterTx; reads on d.ro(); strictly LESS writer contention (this is the fix). Fallible reads check rows.Err()+Close, no nil-deref, no new hot-path write.
- §4 network/auth, §5 MCP surface, §6 lifecycle: untouched.
NITS: two test-only package vars (refScanReadHook/refScanBetweenHook) live in the production file — standard Go test-seam pattern, nil in production, negligible nil-check on a 2-min background path.

## 3. Files changed

```
internal/db/referential_integrity.go      | 426 ++++++++++++++++++++++--------
 internal/db/referential_integrity_test.go | 312 +++++++++++++++++++++-
 2 files changed, 620 insertions(+), 118 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1ac2ce6e-40b6-41d8-a2ab-d282a14e267d`._
