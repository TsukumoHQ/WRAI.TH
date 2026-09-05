# [wraith/relay][split] S3b: projet archivé — refuse-guard au seam unique + allow-to-close + purge exige archivé

## Team : wraith-backend (tsukumo)
## Branch : wraith/archived-guard (from main)
## Relay task : 0a683145-d3e1-4ff9-8d8a-b9347ff6d9a8
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. register_agent, dispatch_task, send_message, claim_task vers un projet archivé -> refus typé explicite (test par tool)
- [ ] 2. complete_task et update_task (sans reassign) d'une task existante non-terminale dans un projet archivé -> autorisés (test)
- [ ] 3. update_task avec assigned_to (reassign) dans un projet archivé -> refusé (test)
- [ ] 4. get_task / list_tasks / lecture memory dans un projet archivé -> autorisés (test)
- [ ] 5. delete_project sur projet ACTIF -> refus 'archive first'; sur projet archivé -> comportement purge existant (test)
- [ ] 6. go test -tags fts5 ./internal/relay/... vert dans le worktree

## 2. Root cause & decisions

ROOT_CAUSE: after S3a added archive/unarchive verbs (projects.archived_at), nothing enforced the archived state — an archived project could still accept NEW work (register_agent, dispatch_task, send_message, claim_task, a task reassign) and delete_project could hard-delete an active project with no archive step. The freeze half of DEC-wraith-archive-project-1 §2 was unimplemented.

DECISION: a SINGLE guard, refuseIfArchived, at the guardIdentity write seam (which wraps every mutating tool) returns a typed, non-retryable ARCHIVED_PROJECT refusal (CategoryPermission) for new-work writes into an archived project — so the client PARKS, not hot-loops. register_agent, a bootstrap tool not wrapped by guardIdentity, calls the same guard from its own handler. Allow-to-close is preserved: complete_task, update_task without a reassign, and release pass through so in-flight work finishes; reads never reach the seam (not in mutatingTools) so they always succeed. update_task carrying assigned_to (a reassign = new work) is refused. delete_project now refuses an active project (archive-first; purge requires archived). IsProjectArchived is the fail-open (missing project / read error → false) single source of truth, read via the RO pool.

REJECTED ALTERNATIVES:
- Scatter per-handler archived checks: duplicated logic, drift risk; one seam is auditable.
- Auto-deactivate agents of an archived project: DEC scope is a status flip with ZERO cascade; agents stay as-is.
- Cascade hard-delete on archive: inverts tolerate-don't-cascade + audit immutability (DEC phases 0-2 / phase3); archive deletes nothing.

```
## review-wraith verdict: SHIP
Scope: internal/db/projects.go (IsProjectArchived read), internal/relay/toolset.go (guard + typed error), handlers_agents.go (register_agent guard), handlers_projects.go (delete_project archive-first), archived_project_test.go (6 tests, one per AC). Rebased onto main 5998491.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (735 passed, 12 packages = 729 base + 6 new).

BLOCKERS (must fix before merge):
- none. No schema/migration change (archived_at column landed in S3a). IsProjectArchived reads via d.ro() (RO pool) — no new writer, no per-request write on a hot path. No agentColumns/scanAgent touch. Task-transition guards unchanged. Deliveries/inbox invariants untouched. Guard is fail-open so a read error never wedges a legitimate write.

NITS (non-blocking):
- none. In-lane (relay handlers + db read). [split] title covers the 5-file scope. Staged explicit paths; commit trailer present; branch off main.
```

## 3. Files changed

```
internal/db/projects.go                 |  17 ++++
 internal/relay/archived_project_test.go | 159 ++++++++++++++++++++++++++++++++
 internal/relay/handlers_agents.go       |   3 +
 internal/relay/handlers_projects.go     |   3 +
 internal/relay/toolset.go               |  46 +++++++++
 5 files changed, 228 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `0a683145-d3e1-4ff9-8d8a-b9347ff6d9a8`._
