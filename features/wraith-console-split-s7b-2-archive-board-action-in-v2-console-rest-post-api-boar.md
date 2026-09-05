# [wraith/console][split] S7b-2: archive-board action in v2 console — REST POST /api/boards/{id}/archive + typed BOARD_HAS_LINEAR_TASKS refusal surfaced verbatim, no force, no retry, v1 untouched

## Team : wraith-engine (tsukumo)
## Branch : wraith/console-s7b2-archive-board (from main)
## Relay task : 4c461184-0505-4b5a-8c1a-798506d56bb1
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 REST: POST /api/boards/{id}/archive on a board with ≥1 open source='linear' task returns 403, Content-Type application/json, body parses with .code == 'BOARD_HAS_LINEAR_TASKS' and .error == the LinearTasksOnBoardError message verbatim (count + remedy); no task archived_at written (httptest)
- [ ] 2. AC2 REST: same route on a native-only board returns 200 and the board is archived with its tasks (DB assertion); unknown board -> 404 JSON body with .error non-empty (httptest)
- [ ] 3. AC3 v2: api.js exposes archiveBoard(project, id) through sendJSON; the v2 module renders the thrown message verbatim inline for the refused board, keeps the board row, offers no force/override control and performs no retry (grep: zero 'force' param on the route call, zero retry loop; documented in the PR with the exact DOM snippet)
- [ ] 4. AC4 v1 untouched: `git diff --stat main -- internal/web/static ':!internal/web/static/v2'` = 0 lines; handlers_boards.go and internal/db unchanged (diff cited)
- [ ] 5. AC5 go test -tags fts5 ./... green (count cited), TestNoDynamicJSONErrorLiterals + TestToolSchemaBudget unchanged and green; ≤5 files incl. scribe md

## 2. Root cause & decisions

# S7b-2 — archive-board action in the v2 console + typed REST refusal

ROOT_CAUSE: S7b-1 (9e0d14fb) put a fail-closed guard in db.ArchiveBoard/DeleteBoard that refuses with a typed *LinearTasksOnBoardError when a board still holds open Linear-mirrored tasks (DEC-wraith-boards-linear-guard-1). But that guard was only reachable through the MCP tool (HandleArchiveBoard) — the v2 console had NO way to archive a board: REST exposed only GET /boards + GET /boards/all (api.go), and v2/api.js is "Read-only" for boards. So a console operator could never trigger the guard, and any future wiring that hit a non-typed error path would have swallowed the reason. This slice adds the missing REST route + one v2 action, surfacing the refusal verbatim.

DECISION (GO cto-tsukumo brief #3, NARROW to audit b983684b §6 wiring):
- REST: POST /api/boards/{id}/archive in internal/relay/api.go (apiArchiveBoard) → r.DB.ArchiveBoard(project, id). project via projectFromRequest (query, like apiGetBoards). *db.LinearTasksOnBoardError → 403 JSON {error: lt.Error() verbatim, code: "BOARD_HAS_LINEAR_TASKS"}; unknown board → 404 JSON; other → 500 JSON; success → 200 {archived:true, board_id}.
- The 403/404 bodies go through a LOCAL jsonErrorCode writer (json.Marshal of a map, mirroring jsonError in json_error.go) — never a hand-built literal, so TestNoDynamicJSONErrorLiterals stays green AND the file budget stays at 5 (no json_error.go edit).
- 404 is decided handler-side from r.DB.ListBoards: db.ArchiveBoard returns nil (no-op) on a missing board, and internal/db must not change, so existence is checked in the handler.
- v2: api.js archiveBoard(project,id) via sendJSON (throws j.error verbatim) + boards(project). settings.js gains a "Boards" cfg-group: a project picker, one row per active board with an Archive button, the server refusal rendered VERBATIM inline, the row kept until a 200 (no optimistic removal), NO force/override control, NO automatic retry. On refusal it best-effort lists the offending open mirrored tasks from /api/tasks/all filtered source='linear' && board_id (no new endpoint).

REJECTED ALTERNATIVES:
- A new dedicated v2 module (boards.js) + router registration in v2.js: would touch 2 files (module + v2.js) and blow the ≤5 budget; the ticket said reuse ONE existing module (settings.js or home). settings.js hosts the cfg-group cleanly.
- A new REST endpoint to list a board's offending tasks: the S7b-1 refusal carries only the count; the ticket says derive the list client-side from existing board/tasksAll data — no endpoint growth.
- jsonErrorCode in json_error.go: natural home + guard-exempt, but adding it there makes 6 files. A local writer in api.go is explicitly allowed by the brief and keeps the budget.

[LEGACY_OPPORTUNITY] none — additive wiring over an existing guard; no dead code uncovered.

## review-wraith verdict: SHIP
Scope: internal/relay/api.go (new POST /api/boards/{id}/archive + local jsonErrorCode), internal/relay/api_test.go (AC1/AC2 httptest), internal/web/static/v2/api.js + v2/settings.js (archive action + Boards group). No sqlite/schema/messaging/MCP-registry/updater surface touched.
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK (no drift) / test -tags fts5 OK (774 passed, guards TestNoDynamicJSONErrorLiterals + TestToolSchemaBudget green).

BLOCKERS (must fix before merge):
- none.

NITS (non-blocking):
- 404 for an unknown board is derived from ListBoards (active set); an already-archived board id would also read as 404 rather than 200-noop. Acceptable within the "no internal/db change" constraint and untested by the ACs; documented in the decision doc.

## 3. Files changed

```
internal/relay/api.go              | 60 ++++++++++++++++++++++++++
 internal/relay/api_test.go         | 88 ++++++++++++++++++++++++++++++++++++++
 internal/web/static/v2/api.js      |  6 +++
 internal/web/static/v2/settings.js | 86 ++++++++++++++++++++++++++++++++++++-
 4 files changed, 239 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `4c461184-0505-4b5a-8c1a-798506d56bb1`._
