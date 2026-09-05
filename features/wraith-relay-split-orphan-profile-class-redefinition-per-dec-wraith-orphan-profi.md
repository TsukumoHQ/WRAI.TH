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
internal/db/referential_integrity.go      |  10 ++-
 internal/db/referential_integrity_test.go | 116 ++++++++++++++++++++++++++++++
 2 files changed, 124 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `96eb1fa3-6f77-4f6c-8773-1fa716003316`._
