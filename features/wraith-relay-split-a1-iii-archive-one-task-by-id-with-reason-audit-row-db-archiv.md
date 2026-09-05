# [wraith/relay][split] A1-iii: archive ONE task by id with reason + audit row (db ArchiveTask + REST POST /api/tasks/{id}/archive) + trovex doc listing the 49 stale skills-registry-v2 ids — code + doc only, batch run is cto-tsukumo post-redeploy

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/archive-task (from main)
## Relay task : 6454127c-551a-4a28-9b4b-7ff4a91593f5
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. db.ArchiveTask stamps archived_at on any non-archived task regardless of status and records exactly one audit_log row (action task.archived, actor, resource_id=task id, reason) — test
- [ ] 2. db.ArchiveTask is idempotent: second call returns false with no new audit row; empty reason returns an error and writes nothing — test
- [ ] 3. REST POST /api/tasks/{id}/archive returns 400 (missing reason), 404 (unknown id), 409 (already archived), 200 {archived:true} — api test covers all four
- [ ] 4. No MCP tool or description change: internal/relay/tools.go untouched, TestToolSchemaBudget unchanged
- [ ] 5. trovex doc 'A1-iii — stale skills-registry-v2 cohort' exists with exactly 49 ids matching the A1-i table, live relay.db mtime identical before/after (read-only copy), and the not-yet-run batch command
- [ ] 6. go build ./..., go vet ./..., go test -tags fts5 ./... green

## 2. Root cause & decisions

ROOT_CAUSE: The relay had only a BULK archive (ArchiveTasks — status/board filtered, no reason, no per-task audit) and no way to archive ONE task by id with a recorded "why". The 49 stale skills-registry-v2 tasks (A1-i anomaly #3) need a reversible, audited, idempotent per-task archive that leaves no schema/MCP cost — hence a REST-only seam over the existing tasks.archived_at column.

## review-wraith verdict: SHIP
Scope: internal/db/task_archive.go (new ArchiveTask), internal/relay/api.go (route + apiArchiveTaskById), internal/db/task_archive_test.go, internal/relay/api_test.go. Trovex cohort doc 0789a37b4e1247c8a004b822a1b55589 (doc only, live relay.db read-only copy, mtime 1788635901 before==after).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (783 passed, 12 pkgs; 6 new tests green).

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/relay/api.go: audit write in ArchiveTask is best-effort (error ignored) per RecordAudit's contract, so a 200 could in theory return with no audit row if the audit INSERT failed. Matches the "audit never blocks the action" rule and the single-writer path makes failure unreachable in practice; noted for completeness.

## Constraint self-check
- single-writer: ArchiveTask uses d.writerExec only (CAS UPDATE + RecordAudit); reads via d.ro()/GetTask. No second writer, no hot-path per-request write (explicit admin action).
- TOCTOU: archive is a guarded conditional UPDATE (WHERE archived_at IS NULL, RowsAffected checked); handler's GetTask-then-CAS lost race still yields 409, never a double-stamp or double-audit. Status is untouched — not a lifecycle transition.
- schema: ZERO change (archived_at pre-exists, used by ArchiveTasks). agentColumns/scanAgent untouched.
- MCP: no tool added; tools.go + TestToolSchemaBudget untouched (REST only).
- route: new case in ServeAPI switch, POST, placed before the generic /tasks/ PUT/DELETE/GET cases; sits behind the CORS→RateLimit→BodyLimit→Auth chain unchanged.
- backward-compatible: old DBs already carry archived_at; old binaries ignore the new route.

## 3. Files changed

```
internal/db/task_archive.go      |  51 +++++++++++++++++
 internal/db/task_archive_test.go | 117 +++++++++++++++++++++++++++++++++++++++
 internal/relay/api.go            |  57 +++++++++++++++++++
 internal/relay/api_test.go       |  65 ++++++++++++++++++++++
 4 files changed, 290 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `6454127c-551a-4a28-9b4b-7ff4a91593f5`._
