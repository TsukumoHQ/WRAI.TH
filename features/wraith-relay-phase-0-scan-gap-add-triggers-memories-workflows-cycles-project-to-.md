# [wraith/relay] Phase 0 scan gap: add triggers/memories/workflows/cycles.project to refChecks()

## Team : wraith-backend (tsukumo)
## Branch : wraith/scan-gap-project-refs (from main)
## Relay task : faea9d73-9e19-476d-99ad-d53680b61566
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. refChecks() covers triggers.project, memories.project, workflows.project, cycles.project
- [ ] 2. seeded-orphan test per new class asserts exact quarantine counts
- [ ] 3. sentinel-named refs in the 4 tables produce zero quarantine rows (tested)
- [ ] 4. scan re-run on the same fixture changes nothing (idempotency test)
- [ ] 5. go test -tags fts5 ./internal/db/... green in the worktree

## 2. Root cause & decisions

# Phase 0 scan gap: triggers/memories/workflows/cycles.project (faea9d73)

ROOT_CAUSE: refChecks() (internal/db/referential_integrity.go) enumerates every FK-shaped column the Phase 0 scan quarantines, but 4 tables added after the original scan was written carry a `project` column that was never wired in — triggers (33 live orphans), memories (3), workflows (3), cycles (1), per audit 494b6323 and ruled GO in DEC-wraith-referential-integrity-phase3-1 §7-Q3. Those orphans were invisible to `integrity_quarantine` even though the audit had already counted them.

## Decision
Add 4 refCheck entries — `orphan_trigger_project`, `orphan_memory_project`, `orphan_workflow_project`, `orphan_cycle_project` — following the exact `orphan_task_project` shape (LEFT-anti-join against `projects.name`, row flagged when non-empty and no matching project). Observability only: detection + quarantine row, no enforcement, no healing, no schema change (the quarantine side-table already handles arbitrary (table,row_id,class)).

- memories additionally excludes `archived_at IS NOT NULL` rows, matching the table's own soft-delete semantics (same pattern the task-orphan checks use for `archived_at`).
- No sentinel-allowlist guard needed here (unlike dispatcher/assignee/sender checks): the ref column is `project`, not an agent/principal field, so `{linear,cron,user}` sentinels are not valid values and never need exempting.

Rejected alternative: a single generic "any table with a project column" reflection-based check — rejected, breaks the existing refChecks() pattern of one explicit, greppable entry per class and would obscure per-table semantics (e.g. the memories archived-at exclusion).

## Invariants held
- No enforcement/healing added — quarantine rows are additive observability, matching Phase 0 semantics for every other class.
- No schema change beyond the existing quarantine side-table.
- ≤5 files touched (2: referential_integrity.go, referential_integrity_test.go).

## Verification
- `go test -tags fts5 ./internal/db/...` — 257 passed, green in the worktree.
- `go build -tags fts5 ./...` clean; gofmt clean.
- New tests: seeded-orphan test per new class (exact quarantine count), sentinel/live-project non-orphan assertions (incl. archived-memory exclusion), and an idempotency assertion (re-run leaves counts unchanged) extending the existing `TestReferentialScanIdempotent`.

## review-wraith verdict: SHIP
Scope: internal/db/referential_integrity.go (4 new refCheck entries), internal/db/referential_integrity_test.go (seed helpers + assertions for the 4 classes)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (257 passed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none — pure additive detection-only scan entries, same LEFT-anti-join shape as the existing orphan_task_project check; no schema, writer, auth, or concurrency surface touched.

## 3. Files changed

```
internal/db/referential_integrity.go      |  35 ++++++++++
 internal/db/referential_integrity_test.go | 103 ++++++++++++++++++++++++++----
 2 files changed, 127 insertions(+), 11 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `faea9d73-9e19-476d-99ad-d53680b61566`._
