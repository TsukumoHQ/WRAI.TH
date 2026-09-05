# [wraith/console][split] FOUNDER: boards dans la console v2 — sélecteur de board + kanban filtré par board_id (parité v1)

## Team : wraith-engine (tsukumo)
## Branch : wraith/v2-boards (from main)
## Relay task : 515a10e7-a8b7-4694-9302-8994106dea96
## Status : 🔵 IN REVIEW

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 board.js appelle api.boards(project) et rend un sélecteur: 'Tous' + boards actifs + '(sans board)' — asset test asserte le call + le sélecteur
- [ ] 2. AC2 sélection d'un board filtre le kanban par board_id client-side; 'Tous' = comportement actuel inchangé — test
- [ ] 3. AC3 carte task affiche le chip nom-de-board quand board_id résolu; board_id dangling = pas de chip, pas d'erreur — test
- [ ] 4. AC4 switch de projet reset la sélection + refetch — test ou assertion structurelle
- [ ] 5. AC5 go build/vet/test -tags fts5 ./... green (count cité); tests v2 existants (delete/config/project-select) inchangés et verts; ≤2 fichiers non-test

## 2. Root cause & decisions

# Boards in the v2 console (515a10e7)

ROOT_CAUSE: FOUNDER directive "mettre tous les boards sur la V2". Boards exist in the data and in v1, but the v2 kanban (internal/web/static/v2/board.js) was board-blind — zero board_id references — so the boards dimension was invisible in v2. Not a defect; a parity gap between the v2 console and the underlying board-partitioned task data.

DECISION: Add the boards dimension to v2 ONLY (two-UI doctrine — v1 static/index.html untouched), reusing the pre-existing REST route GET /api/boards (api.go:175) and the pre-existing api.js wrapper `boards(project)`. No new REST route, no api.js change, no schema change. All changes land in board.js (single non-test source file) plus a Go asset test.

- Load `ctx.api.boards(project)` in load() alongside cycles/profiles (single-project only; the all-projects view stays board-less). Build `boardById` (id → name).
- Board selector inserted into `.board-controls` in JS (no HTML template edit), reusing the cycle-filter pill styling (zero CSS): 'Tous' (default = current behaviour, unchanged) + one pill per active board + '(sans board)' for tasks with an empty or dangling board_id, with per-board task counts.
- Client-side board_id filter inside applyFilters (the single kanban filter choke): 'Tous' is a no-op; a specific board keeps `board_id === boardSel`; '(sans board)' keeps empty AND dangling ids (id not among the fetched boards).
- Board-name chip on the card, gated on a resolved name from boardById — a dangling/empty board_id yields no chip and never throws (DEC-wraith-referential-integrity tolerance: dangling refs are tolerated, never cascaded or crashed on).
- A project switch (load's resetCycle path, which ctx.onSelection triggers via load(true)) resets the selection to 'Tous' and refetches boards.

REJECTED ALTERNATIVES:
- Adding a board-selector element to index.html — rejected: keeps the change to board.js only (the constraint's "idéalement board.js seul"); the element is created in JS and inserted into the existing board-controls.
- New CSS classes for the selector/chip — rejected: reused `.cyc-pill` (selector) and `.lchip` (chip) so no v2.css change, staying at one non-test file.
- Server-side board filtering / a new route — rejected: GET /api/boards already returns the boards and the kanban already holds every task client-side; filtering is a pure client concern, and the ticket explicitly forbids a new route.
- Putting archive/create of a board in board.js — rejected: those stay in settings (S7b-2) per scope.

SCOPE: v2 frontend + one Go asset test. Zero new route, zero schema change, api.js unchanged, v1 untouched. ≤2 non-test files (board.js only).

## review-wraith verdict: SHIP
Scope: internal/web/static/v2/board.js (board selector + client-side board_id filter + board-name chip), internal/relay/console_v2_boards_test.go (5 subtests, AC1-5).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (803 passed; existing v2 delete/config/project-select tests unchanged and green).

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- Client-side behaviour (pill click → filter, chip render) is pinned at the source level only; a true DOM/runtime assertion would need a JS harness (none exists in this repo), same limitation the console_v2_delete_test.go states.

No new REST route (GET /api/boards pre-exists), no api.js change, no schema change, v1 (static/index.html) untouched, single-writer/handlers unaffected (frontend-only + a read-path asset test).

## 3. Files changed

```
...dans-la-console-v2-s-lecteur-de-board-kanban.md |  67 +++++++++++
 internal/relay/console_v2_boards_test.go           | 130 +++++++++++++++++++++
 internal/web/static/v2/board.js                    |  64 +++++++++-
 3 files changed, 258 insertions(+), 3 deletions(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-515a10e7-a8b7-4694-9302-8994106dea96 @ `e566c02f3`
- 🟢 AC1: asset marker pin + live endpoint assertion both green — evidence: board.js:62 calls ctx.api.boards(sel); lines 318-320 render Tous / per-board / (sans board) pills; live REST exercised via doAPI(r, GET /boards?project=p1) → 1 board returned — test: TestV2Boards/BoardsCallAndSelector internal/relay/console_v2_boards_test.go:21
- 🟢 AC2: default + no-op branch + board_id match + applyFilters order all pinned — evidence: board.js:42 boardSel = all default; lines 122-129 filter branch inside applyFilters with boardSel !== all as no-op guard; order check af<branch<next passes — test: TestV2Boards/ClientSideBoardFilter internal/relay/console_v2_boards_test.go:56
- 🟢 AC3: Map construction + get() lookup + conditional chip + kcard-board marker all pinned; dangling yields undefined which is falsy in conditional — evidence: board.js:66 boardById = new Map((boards).map b=>[b.id,b.name]); line 233 boardName = t.board_id ? boardById.get(t.board_id) : empty; line 251 conditional chip render — test: TestV2Boards/BoardChipResolvedDanglingSafe internal/relay/console_v2_boards_test.go:78
- 🟢 AC4: resetCycle reset + load(true) wiring + boards refetch all pinned — evidence: board.js:68 if (resetCycle) boardSel = all; line 62 ctx.api.boards(sel) in load; line 734 ctx.onSelection calls load(true) — test: TestV2Boards/ProjectSwitchResetsAndRefetches internal/relay/console_v2_boards_test.go:97
- 🟢 AC5: gate green + suite green + scope ≤2 non-test files — evidence: go test -tags fts5 -count=1 ./... → 803 passed in 12 packages; TestV2Boards (6) + TestV2Delete/Config/ProjectSelect (20) all green; git diff name-only = 3 files (board.js, console_v2_boards_test.go, feature md); 1 non-test src — test: go test ./... + TestV2Boards + existing v2 suite

## 5. Timeline

- round 1 → **approve** (review-515a10e7-a8b7-4694-9302-8994106dea96)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `515a10e7-a8b7-4694-9302-8994106dea96`._
