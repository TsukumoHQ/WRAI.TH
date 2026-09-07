# [wraith/db] RollupTokenUsage must not hold the single writer: aggregate on the reader pool, one short chunked-upsert tx, re-sum only days that can still change + daily full catch-up

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/rollup-off-writer (from main)
## Relay task : a0663508-597e-4872-8cc0-227cab3e7cd6
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: while the rollup's aggregate read is in progress (fixture with a slow/paused aggregate hook), a concurrent writerExec from another goroutine completes within 100ms; revert-check: running the aggregate on the writer conn makes the concurrent write wait
- [ ] 2. AC2 named test: rollup output byte-identical to today's on fixtures spanning >= 3 days incl. a fully-purged day (its stored aggregate never overwritten) and an ON CONFLICT update of an existing day row; the non-shrinking-history comment block preserved
- [ ] 3. AC3 named test: with writerSlowWait shrunk to 20ms and a slow aggregate fixture, no `writer wait` line attributable to the rollup is logged (the apply tx alone is under threshold); the apply tx is opened via beginWriterTx
- [ ] 4. AC4 in-diff: the routine tick re-sums only rows with created_at >= yesterday 00:00Z (named test: a row for day-3 inserted late is NOT corrected by the routine tick) and the only statements on the writer are INSERT ... ON CONFLICT DO UPDATE against token_usage_daily with explicit values (no SELECT ... FROM token_usage inside the apply tx) — grep-able assertion or named test
- [ ] 5. AC5 named test: the daily full catch-up tick re-sums the full 14-day retention window and fixes the late day-3 row from AC4; scope: diff touches at most internal/db/token_usage.go, internal/db/token_usage_test.go, internal/relay/cleanup.go; go test -tags fts5 ./internal/db/... green

## 2. Root cause & decisions

# Decision — ticket C a0663508 (RollupTokenUsage off the single writer)

## ROOT_CAUSE
`RollupTokenUsage` ran one `INSERT ... SELECT ... GROUP BY` over the whole raw
retention window on the coord **writer** connection. That SELECT builds a TEMP
B-TREE over hundreds of thousands of rows (seconds under CPU load) and pinned the
single `SetMaxOpenConns(1)` writer for its full duration, starving every other
relay write into `writerTimeout`.

## FIX (3 files, AC5 scope)
- `internal/db/token_usage.go`: split into read-then-apply (`rollupTokenUsageSince`).
  READ = full GROUP BY on the reader pool `d.ro()` into `[]rollupRow` (holds no
  writer). APPLY = one short `beginWriterTx` with a prepared
  `INSERT ... ON CONFLICT DO UPDATE` on explicit values (milliseconds; no SELECT on
  the writer). `len(groups)==0 → return nil` preserves the non-shrinking-history
  invariant (a fully-purged day yields no group, its stored aggregate is untouched).
  `RollupTokenUsage(retentionDays)` kept as the FULL-window rollup (byte-identical
  semantics to pre-split); new `RollupTokenUsageRoutine()` = yesterday-00:00Z-onward
  cheap tick. `rollupReadHook` = nil-in-prod test seam.
- `internal/relay/cleanup.go`: routine every tick; full `RollupTokenUsage` catch-up
  once per UTC day (`lastRollupCatchup` guard) to fold in late rows for older days.
  Rollup still runs BEFORE purge.
- `internal/db/token_usage_test.go`: 5 tests, one per AC.

## NOTE (naming, for PR)
`RollupTokenUsage` was NOT renamed to the routine despite the ticket's literal
wording: `token_usage_split_test.go` (out of AC5 scope) pins
`RollupTokenUsage(14)`=full-window. The cheap 5-min tick is the new
`RollupTokenUsageRoutine`; prod goal (cheap tick off the writer) still met.

## review-agent-runtime verdict: SHIP
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK
  (internal/db 12.5s, internal/relay 13.2s); 5 C tests -race -count=3 = 15 pass.
Single-writer discipline: aggregate on `d.ro()`, apply on `beginWriterTx`
(= `d.conn.Conn()`, the single writer pool). No second writer, no per-request
hot-path write. Ordering (rollup before purge) preserved. Only cleanup.go calls the
rollups; no stale INSERT...SELECT callers. Test hook nil-reset at both sites.
BLOCKERS: none.
NIT (non-blocking): first cleanup tick runs routine + full catch-up both; harmless,
idempotent.

## 3. Files changed

```
internal/db/token_usage.go      | 105 ++++++++++++++++++--
 internal/db/token_usage_test.go | 206 ++++++++++++++++++++++++++++++++++++++++
 internal/relay/cleanup.go       |  13 ++-
 3 files changed, 316 insertions(+), 8 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `a0663508-597e-4872-8cc0-227cab3e7cd6`._
