# [wraith/relay][split] A1-ii: limbo sweeper — block >7d / archive >30d, INACTIVE-inclusive, dry-run default ON

## Team : wraith-engine (tsukumo)
## Branch : wraith/relay-a1ii-limbo-sweeper (from main)
## Relay task : bc350cb8-9535-484c-897a-bd5290782b07
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. dry-run sur la COPIE de relay.db reproduit la disposition A3 baseline du doc 0a2a422c (85 rows: tier attribué par row, T1/T2 annulées exclues) — sortie collée dans le doc trovex de la task
- [ ] 2. tests: inactive vs active vs is_service (service jamais touché); staleness sur les deux horloges (task récente OU agent récent = keep); re-run idempotent (déjà 'assignee inactive' = skip); digest groupé par dispatcher + suppression si dispatcher inactif; tier 2 seulement si dispatcher non-actif; dry-run = zéro écriture (compte rows avant==après)
- [ ] 3. tier 1 passe par la transition CAS normale, blocked_reason exact 'assignee inactive: <name> (last_seen <date>)'; tier 2 = archived_at seul, zéro delete
- [ ] 4. jamais de reassign, aucun UPDATE assigned_to dans le diff
- [ ] 5. flag dry-run défaut ON documenté (comment le passer OFF); lignes journal préfixe 'integrity:'
- [ ] 6. go test -tags fts5 ./internal/db/... ./internal/relay/... vert; ≤5 fichiers

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/db/limbo_sweep.go (new), internal/relay/task_sweeper.go (+sweepLimboAssignees/digestLimboBlocks/limboSweepApply), internal/db/limbo_sweep_test.go (new), internal/relay/limbo_sweep_test.go (new).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (742 passed, 12 pkgs). Rebased on main 846c6b1.

ROOT_CAUSE: A task claimed by an agent that later goes inactive stays owned forever. The lease sweep (SweepExpiredLeases) only requeues an EXPIRED lease of a dead holder to pending; a non-pending claimed task whose assignee just went quiet is never converged — it sits held by a ghost, un-pickable. A1-i proved the original GONE-only rule was a no-op on the real limbo set (all 85 assignees are INACTIVE, not GONE). A1-ii closes the class INACTIVE-inclusive, reversibly and safely.

DESIGN (per ruling 8fdb5170, option A):
- Single-writer discipline: read cursor drained+closed BEFORE any write; every write a CAS-guarded writerExec (WHERE status=fromStatus / archived_at IS NULL), RowsAffected==0 = raced no-op. No second writer, no per-request hot-path write (rides the 2-min maintenance ticker).
- TIER 1 (>7d both clocks): status=blocked, blocked_reason='assignee inactive: <name> (last_seen <ts>)'. blockLimboTask is the "équivalent single-writer" the ruling allows (BlockTask's validTransitions forbids accepted→blocked, which is exactly a limbo case).
- TIER 2 (>30d both clocks AND dispatcher gone/inactive>30d): archived_at only — nullable, zero delete, fully reversible.
- Both-clock staleness (task last_activity_at AND agent last_seen); created_at never used; a missing timestamp is not staleness.
- NEVER reassign (DEC-wraith-update-task-reassign-1); no UPDATE assigned_to in the diff.
- Dry-run DEFAULT ON: env RELAY_LIMBO_SWEEP_APPLY (1/true/yes to apply, 0/false/no to force dry-run) wins over DB setting limbo_sweep_apply='1'. Journal lines use the 'integrity:' prefix (logging.go untouched).
- Digest: one per dispatcher per sweep, sent only if the dispatcher is active; gone/inactive dispatcher = journal line, never a message into the void.

EVIDENCE: dry-run reproduction on a READ-ONLY copy of live relay.db (live mtime BEFORE==AFTER 2026-09-05T16:08:21Z, zero write): 83 scanned → 13 block + 57 archive + 13 keep; the 13 keep are assignee-seen-<7d (both-clock guard); the two since-cancelled baseline ids (d43a2a6a T1, 17ea84dc T2) correctly out of set (85−2=83). Full output: trovex 0614432fda764926893ea1bdb6704ac5.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
internal/db/limbo_sweep.go         | 263 +++++++++++++++++++++++++++++++++++++
 internal/db/limbo_sweep_test.go    | 256 ++++++++++++++++++++++++++++++++++++
 internal/relay/limbo_sweep_test.go |  84 ++++++++++++
 internal/relay/task_sweeper.go     |  91 +++++++++++++
 4 files changed, 694 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `bc350cb8-9535-484c-897a-bd5290782b07`._
