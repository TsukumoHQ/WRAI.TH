# [wraith/relay][split] limbo sweep flip-gate fixes: already-blocked rows skipped everywhere, AgeDays correct for second-precision timestamps

## Team : wraith-engine (tsukumo)
## Branch : wraith/limbo-flip-gate (from main)
## Relay task : e16b495c-df90-405f-8876-3152f2783d5d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 a candidate row with status='blocked' and a NON-limbo blocked_reason is skipped in apply mode: blocked_reason unchanged, zero audit rows, not in res.Blocked — test
- [ ] 2. AC2 the same row in dry-run emits NO shadow line and is not in res.Blocked — test
- [ ] 3. AC3 limboAgeDays returns the true whole-day age for a no-frac timestamp (e.g. 2026-07-23T12:19:03Z vs now 2026-09-05 = 44) AND for a frac-seconds timestamp; unparseable and future stamps still return 0 — test
- [ ] 4. AC4 TestLimboSweepIdempotent and TestLimboSweepDryRunWritesNothing still green
- [ ] 5. AC5 go build ./..., go vet ./..., go test -tags fts5 ./... green (count cited); internal/relay/tools.go untouched

## 2. Root cause & decisions

# Limbo sweep flip-gate fixes (e16b495c)

ROOT_CAUSE: Two independent defects surfaced in the pre-flip shadow observation of the limbo sweep (internal/db/limbo_sweep.go).

1. Provenance loss on already-blocked rows. The candidate SQL prefilter accepts any non-terminal task (`status NOT IN ('done','cancelled')`), so `status='blocked'` rows are scanned. The only blocked-skip guard was `r.status == "blocked" && strings.HasPrefix(r.blockedReason, limboReasonPrefix)` — a task blocked for ANY OTHER reason (e.g. a lead-set reason) fell through to the block path, where blockLimboTask would overwrite its original blocked_reason with the limbo-sweep reason, destroying provenance.

2. Wrong age for second-precision timestamps. limboAgeDays parsed with memoryTimeFmt = "2006-01-02T15:04:05.000000Z". Go's zero-padded `.000000` layout requires EXACTLY 6 fractional digits, so a plain stamp like `2026-07-23T12:19:03Z` (44 days old) failed to parse and fell back to 0 — the shadow line reported `0d` for genuinely stale rows.

DECISION:
1. Skip ANY task with `status == "blocked"` before the block. A blocked task is out of limbo by definition (a ghost-held task is non-blocked). This subsumes the limbo-prefix idempotence check (re-running stays a no-op) while preserving every original blocked_reason. Skipped = no shadow line, no block, no audit row. The now-unused `strings` import is dropped.
2. Parse limboAgeDays with time.RFC3339, which accepts BOTH fractional-second and second-precision stamps. The future-stamp and unparseable guards still return 0. memoryTimeFmt itself is UNCHANGED — other callers depend on it; only this age display is widened.

ELIGIBILITY UNAFFECTED by (2): the block cut (`lastActivityAt < blockCut`) is a lexicographic string compare, not a parse, so a no-frac stamp is still correctly judged stale.

REJECTED ALTERNATIVES:
- Widening memoryTimeFmt globally — rejected: it is shared by other callers (memories.go:17); a format change there is out of scope and risky. Scope the parse change to limboAgeDays only.
- Keeping the prefix check and only skipping limbo-prefixed blocks — rejected: leaves the provenance bug for non-limbo blocked rows, which is exactly what shadow observation caught.

SCOPE: db-layer only. Zero schema change, tools.go untouched (TestToolSchemaBudget unchanged), single-writer discipline and digestLimboBlocks unchanged.

## review-wraith verdict: SHIP
Scope: internal/db/limbo_sweep.go (skip-any-blocked guard + limboAgeDays RFC3339 parse), internal/db/limbo_sweep_test.go (AC1/AC2/AC3 tests).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (788 passed).

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- Pre-existing (not this diff): the eligibility block cut is a lexicographic compare of a raw stamp against a memoryTimeFmt-formatted blockCut; a no-frac stamp exactly at the boundary sorts after the ".000000Z" cut. Immaterial (strict-less, sub-second) and explicitly out of scope per the ticket; noted for completeness.

Single-writer intact (blockLimboTask CAS unchanged, no new writer), zero schema change, tools.go untouched (TestToolSchemaBudget unchanged), memoryTimeFmt constant unchanged, backward-compatible.

## 3. Files changed

```
internal/db/limbo_sweep.go      | 29 ++++++++-----
 internal/db/limbo_sweep_test.go | 92 +++++++++++++++++++++++++++++++++++++++++
 2 files changed, 111 insertions(+), 10 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `e16b495c-df90-405f-8876-3152f2783d5d`._
