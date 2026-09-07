# [wraith/db] drop LOWER() from the referential scan's agent/profile lookups + lowercase-invariant test — after write-path normalization lands

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/scan-lower-drop (from main)
## Relay task : 790fe231-eb55-45a9-a2f6-784fdd7b75c9
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 in-diff: no `LOWER(a.name)`, `LOWER(p.slug)`, `LOWER(a.profile_slug)` remains in refChecks(); sentinel NOT IN clauses unchanged
- [ ] 2. AC2 named test: scan output on the existing 13 fixtures byte-identical (open counts, detect/heal/reopen sets, log lines) before/after the LOWER drop
- [ ] 3. AC3 named test: the case-invariant check returns 0 violations on the fixtures and reports n=1 when a mixed-case agent row is inserted directly via SQL (bypassing the handler) — revert-check: the check removed makes the test fail
- [ ] 4. AC4 named test: with a mixed-case agent row present, the scan logs `integrity: case-invariant violated` once and still completes (never errors, never blocks)
- [ ] 5. AC5 scope: 2 files, go test -tags fts5 ./internal/db/... green

## 2. Root cause & decisions

# Decision — ticket B2 790fe231 (drop LOWER() from referential scan + case-invariant guard)

## ROOT_CAUSE
refChecks() resolved orphan refs with `LOWER(a.name)=LOWER(t.xxx)` /
`LOWER(p.slug)=LOWER(...)` / `LOWER(a.profile_slug)=LOWER(...)`. Wrapping the
indexed column in LOWER() defeats the index: EXPLAIN showed
`SEARCH a USING COVERING INDEX idx_agents_project_name (project=?)` — a per-row
scan of every agent in the project — instead of a `(project=? AND name=?)` point
lookup (measured 2.2x slower: 364ms vs 163ms / 5 execs). The LOWER() was only
needed while writes could store mixed case; task B1 (df850e85, merged) now
lowercases name/slug/assignee/recipient at write, so the wrappers are dead weight.

## FIX (2 files, AC5 scope)
- internal/db/referential_integrity.go: refChecks() resolution clauses use plain
  equality (`a.name=t.xxx`, `p.slug=...`, `a.profile_slug=...`). Kept the cheap
  defensive `LOWER(x) NOT IN (sentinels)` guards (AC1). Added checkCaseInvariant()
  — reader-pool, log-only guard the scan calls once/run after the count phase;
  counts rows where a lowercase-canonical column (agents.name, profiles.slug,
  tasks.assigned_to, messages.to_agent) still holds a non-lowercase value and logs
  `integrity: case-invariant violated table=... n=...` per offending table. Never
  errors the scan, never blocks a write (AC4); returns total for tests.
- internal/db/referential_integrity_test.go: 5 tests (AC5 smoke added). Replaced obsolete
  TestOrphanProfilePoolCaseInsensitive (asserted the case-insensitive resolution
  this change removes) — its cross-project-scoping assertion folded into the AC3
  test.

## Tests (one per testable AC; AC5 = scope, not a runtime test)
- AC1 TestRefChecksDropLowerEquality: no LOWER(a.name/b.name/p.slug/a.profile_slug)
  equality remains; sentinel NOT IN guard survives.
- AC2 TestReferentialScanFixturesByteIdentical: lowercase fixtures resolve/flag
  identically post-drop (pool resolves, dead slug flags, 0 case violations).
- AC3 TestCaseInvariantCheckDetectsMixedCase: 0 on clean lowercase DB (+ project
  scoping), n=1 after a mixed-case row injected directly via SQL. Revert-check:
  asserts checkCaseInvariant's return, so removing the guard breaks the test.
- AC4 TestScanLogsCaseInvariantViolationOnce: mixed-case row -> scan logs the line
  exactly once and completes without error.

- AC5 TestScanLowerDropSuiteSmoke: scope/suite-green smoke — full scan over the
  fixture set completes without error, refChecks() has 16 classes, the 15
  orphan-producing fixtures each report one open row, orphan_claimer stays 0.

## review-agent-runtime verdict: SHIP
Scope: internal/db/referential_integrity.go, internal/db/referential_integrity_test.go (2 files).
§1 no agentColumns/scanAgent touch, no migration, no schema change — checkCaseInvariant reads only existing cols. §2/§3 checkCaseInvariant runs on d.ro() (reader pool), fail-open (continue on err), log-only, once per scan (not a per-request hot path) — holds no writer; scan read/apply structure from 1ac2ce6e untouched. SQL uses compile-time-const table/col (no injection). §4/§5/§6 N/A (no bind/route/tool/lifecycle change).
BLOCKERS: none.
NITS: none.
Gate: go test -tags fts5 ./internal/db/... = 322 pass; 5 AC tests pass isolated.
§1 no agentColumns/scanAgent/migration/schema change. §2 no state transition;
checkCaseInvariant read-only on d.ro(); no shared in-memory state. §3 single-writer
intact — check on the RO pool, holds no writer; no per-request hot-path write (runs
in the periodic scan); fails open (err -> continue, no panic). LOWER-drop safe:
ticket evidence = 0 mixed-case rows on the live DB (all four columns), and any
future drift is now surfaced by the guard. §4-6 untouched.
BLOCKERS: none.
NIT (non-blocking): the 4 `col <> LOWER(col)` counts are non-indexable full-table
scans each scan run — fine at current cadence/scale; revisit if these tables grow
very large.

## 3. Files changed

```
internal/db/referential_integrity.go      |  65 ++++++++++--
 internal/db/referential_integrity_test.go | 166 ++++++++++++++++++++++++++++--
 2 files changed, 212 insertions(+), 19 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `790fe231-eb55-45a9-a2f6-784fdd7b75c9`._
