# [wraith/relay][split] S3a: archive_project — schéma archived_at + verbes archive/unarchive + filtre ListProjects + test invariant §4

## Team : wraith-backend (tsukumo)
## Branch : wraith/archive-project (from main)
## Relay task : 68180529-d860-4efe-a61a-64f2b219fae1
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. projects.archived_at ajouté via ensureColumns; ouverture d'une DB existante = zéro erreur, toutes rows actives (test)
- [ ] 2. archive_project flip archived_at en une tx; déjà archivé -> erreur explicite; inconnu -> erreur; unarchive remet NULL; aucune autre table touchée (test compte rows avant/après)
- [ ] 3. ListProjects + ListProjectsWithInfo excluent les archivés par défaut; variante includeArchived les renvoie avec archived_at renseigné (test)
- [ ] 4. archive_project refuse si le projet est dans linear_project_map (test)
- [ ] 5. INVARIANT §4: archiver un projet avec tasks/memories vivantes puis scan d'intégrité = zéro nouvelle row orphan_*_project (test de régression nommé)
- [ ] 6. go test -tags fts5 ./internal/db/... ./internal/relay/... vert dans le worktree

## 2. Root cause & decisions

# S3a: archive_project — soft-archive (schema + verbs + list filter)

ROOT_CAUSE: There was no non-destructive way to retire a stale/dead project from the relay. The only removal path, delete_project, hard-deletes with a cascade across ~14 tables — which inverts the tolerate-don't-cascade invariant of referential-integrity phases 0-2 and the audit-immutability ruling of phase 3 (DEC-wraith-referential-integrity-phase3-1). Operators need to hide a project from listings reversibly, with zero data loss.

DECISION (DEC-wraith-archive-project-1, cto-tsukumo 2026-09-05, design trovex abf5ab8464b34b639586e392aaaa13cb):
- Add nullable projects.archived_at TEXT via ensureColumns (NULL = active) — additive and old-binary-compatible, mirrors tasks.archived_at and the require_typed_ticket rollout.
- archive/unarchive = a single writer-tx status flip: ZERO cascade, fully reversible. already-archived / unknown / not-archived all return an explicit loud error (never a silent no-op).
- ListProjects[WithInfo] exclude archived by default; *Filtered(includeArchived) variants + ProjectInfo.ArchivedAt expose them when asked.
- orphan_*_project subqueries stay existence-only, so a reference to an archived project is a non-orphan by construction (pinned by TestArchivedProjectRefsNotOrphaned).
- Linear-backed projects (present in linear_project_map) are refused — fail-closed until the S7 boards×Linear safe/forbidden matrix is designed.

REJECTED ALTERNATIVES:
- Hard-delete cascade to remove a project: inverts tolerate-don't-cascade (phases 0-2) and audit immutability (phase 3 NO-GO). The nuclear path (purge) is deferred to S3b and gated on archived-first.
- Side-table for archived state (the Q2 quarantine pattern): the scanTask/scanAgent lockstep rationale that justified a side-table for agent quarantine does not apply to projects — a nullable column is simpler, and there is no hot scan-path lockstep to protect.

SCOPE: this slice is SCHEMA + VERBS + LIST-FILTER only. The refuse-guard at the seam, allow-to-close, and purge-requires-archived are the behaviour half, sequenced in S3b (0a683145) after this merges.

```
## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/{db.go,projects.go,web_queries.go,projects_archive_test.go}, internal/models/project.go, internal/relay/{handlers_projects.go,tools.go,toolset.go}
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (620 passed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/projects.go ArchiveProject: the not-found vs already-archived disambiguation SELECT after RowsAffected==0 is a second read, so a concurrent flip between the UPDATE and the SELECT could mislabel the error message (correctness of the flip itself is unaffected — the guarded UPDATE is atomic).
- internal/relay/tools.go: ~13 unrelated tool/param descriptions were trimmed (meaning-preserving) to keep TestToolSchemaBudget green after +2 tools; the alternative was bumping the budget constant. Documented in the plan/decision note.
```

## 3. Files changed

```
internal/db/db.go                    |   9 ++
 internal/db/projects.go              | 101 ++++++++++++++++-
 internal/db/projects_archive_test.go | 212 +++++++++++++++++++++++++++++++++++
 internal/db/web_queries.go           |  17 ++-
 internal/models/project.go           |   1 +
 internal/relay/handlers_projects.go  |  32 ++++++
 internal/relay/tools.go              |  52 ++++++---
 internal/relay/toolset.go            |   6 +-
 8 files changed, 406 insertions(+), 24 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `68180529-d860-4efe-a61a-64f2b219fae1`._
