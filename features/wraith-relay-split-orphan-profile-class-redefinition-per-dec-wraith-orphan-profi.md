# [wraith/relay][split] orphan_profile class redefinition per DEC-wraith-orphan-profile-burndown-1: pool-key resolver + terminal exclusion

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/orphan-profile-redef (from main)
## Relay task : 96eb1fa3-6f77-4f6c-8773-1fa716003316
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 task whose slug matches NO profiles row but an in-project agents.profile_slug (case-insensitive) is NOT flagged — fixture test
- [ ] 2. AC2 done/cancelled task with fully-dead slug NOT flagged; non-terminal fully-dead slug STILL flagged — fixture tests both directions
- [ ] 3. AC3 previously-quarantined row whose condition cleared under the new definition gets resolved_at stamped on re-scan (heal path) — test
- [ ] 4. AC4 all OTHER class counts unchanged on the existing master fixture — existing count assertions stay green unmodified except orphan_profile expectations
- [ ] 5. AC5 go build ./..., go vet ./..., go test -tags fts5 ./... green (count cited); ≤2 non-test source files

## 2. Root cause & decisions

# .niwa-decision — 96eb1fa3 orphan_profile class redefinition

ROOT_CAUSE: the orphan_profile integrity class (internal/db/referential_integrity.go) flagged a task's profile_slug as orphan whenever no profiles row matched — but the profiles table is sparse; the real agent registry is the agents pool. So every live slug carried only by an agent (not a profiles row) was a false positive, and dead slugs on already-finished (done/cancelled) tasks were counted as live limbo noise. Open ledger stood at 936, almost all false.

DECISION (per DEC-wraith-orphan-profile-burndown-1, ruling on design doc trovex 94e27a349b6a4b97a0080da357e68ac5, Q1+Q2 — class-definition change only, NO data rewrite, NO quarantine-table change):
- Q1 SECOND resolver: a slug is live when a profiles row matches OR any in-project agent carries it. Added `AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.project = t.project AND LOWER(a.profile_slug) = LOWER(t.profile_slug))`. Project-scoped, case-insensitive (LOWER both sides), agents pool = real registry (phase3 Q5).
- Q2 terminal exclusion: added `AND t.status NOT IN ('done', 'cancelled')` (limbo-class precedent :123) — a dead slug on a finished task is noise, not limbo.
- Class comment updated to name both resolvers + the terminal exclusion.
- Reversibility: the existing heal path (:328-357) auto-resolves rows whose condition cleared on the next scan and re-opens regressed rows; a revert simply reopens the rows. No migration, no quarantine schema change.

EXPECTED LIVE EFFECT (cite): open ledger 936 → ~37 true-dead after the next startup scan.

FIXTURE COUNTS: master fixture TestReferentialScanDetectsOrphanClasses orphan_profile stays exactly 1 (t-oprofile: pending, slug 'no-such-profile' carried by no agent) — all other class counts byte-for-byte unchanged. New tests: pool-resolved slug → 0 flagged; fully-dead slug → 1; done+cancelled dead slug → 0; non-terminal dead slug → 1; cross-project agent → does NOT resolve; heal path stamps resolved_at (row kept, not deleted).

## review-wraith verdict: SHIP
Scope: internal/db/referential_integrity.go (class SQL + comment) + internal/db/referential_integrity_test.go (4 new tests). 1 non-test source file (≤2), tests excluded.
Gate: gofmt clean / go build -tags fts5 OK / go vet OK / go test -tags fts5 -count=1 ./... green.
Tests (one per AC): AC1 TestOrphanProfilePoolResolver (+ TestOrphanProfilePoolCaseInsensitive), AC2 TestOrphanProfileTerminalExclusion, AC3 TestOrphanProfileHealPath, AC4 TestReferentialScanDetectsOrphanClasses (existing, unmodified — other classes unchanged), AC5 full suite green + ≤2 non-test files.
BLOCKERS: none.
NITS: none. Other integrity classes byte-for-byte untouched; zero schema change; zero MCP tool change.

## 3. Files changed

```
...ass-redefinition-per-dec-wraith-orphan-profi.md |  56 ++++++++++
 internal/db/referential_integrity.go               |  10 +-
 internal/db/referential_integrity_test.go          | 116 +++++++++++++++++++++
 3 files changed, 180 insertions(+), 2 deletions(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-96eb1fa3-6f77-4f6c-8773-1fa716003316 @ `70cad0f0a`
- 🟢 AC1: pool resolver verified: t-pool resolves via agent wb-agent; t-dead still flags; case-insensitive + project-scoped confirmed — evidence: referential_integrity.go:144 adds AND NOT EXISTS agents pool resolver (LOWER both sides, project-scoped) — test: TestOrphanProfilePoolResolver referential_integrity_test.go:501 + TestOrphanProfilePoolCaseInsensitive :529
- 🟢 AC2: terminal exclusion verified: t-done/t-cancelled NOT flagged, t-open (in-progress) still flags — openCount=1 — evidence: referential_integrity.go:142 adds AND t.status NOT IN ('done','cancelled') — test: TestOrphanProfileTerminalExclusion referential_integrity_test.go:555
- 🟢 AC3: heal path verified: scan 1 flags t-heal; seedAgent newcomer carrying slug; scan 2 resolves — openCount=0 AND resolved_at IS NOT NULL row count=1 (resolved, not deleted) — evidence: referential_integrity.go:140-144 new orphanSQL re-evaluated each scan via existing runReferentialScan heal path — test: TestOrphanProfileHealPath referential_integrity_test.go:582
- 🟢 AC4: master fixture unmodified; AC4 hold verified: other class counts byte-for-byte unchanged; orphan_profile=1 matches new pool resolver (t-oprofile='no-such-profile' carried by no agent) — evidence: referential_integrity_test.go:217-233 want map untouched in diff (0 deleted lines); all 14 non-orphan_profile counts=1, orphan_profile=1 — test: TestReferentialScanDetectsOrphanClasses referential_integrity_test.go:163
- 🟢 AC5: AC5 green: build/vet/test all green; 1 non-test source file (limit ≤2) — evidence: go build EXIT=0; go vet -tags fts5 EXIT=0; go test -tags fts5 ./... all pkgs ok; only internal/db/referential_integrity.go non-test file — test: go test -tags fts5 -count=1 ./... exit 0; 9 pkgs ok / 4 no-test

## 5. Timeline

- round 1 → **approve** (review-96eb1fa3-6f77-4f6c-8773-1fa716003316)

**Approve-with-findings (follow-up):** build/vet/test green; 4 new tests + master fixture all pass; pool resolver + terminal exclusion match plan; 1 non-test source file; no bar weakening

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `96eb1fa3-6f77-4f6c-8773-1fa716003316`._
