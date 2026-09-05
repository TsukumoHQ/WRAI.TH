# [wraith/console][split] S2: bouton delete task dans board.js avec confirmation

## Team : wraith-engine (tsukumo)
## Branch : wraith/console-s2-delete-task (from main)
## Relay task : 654d358d-13c8-431a-99f7-1f60334fe5d6
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. api.js expose deleteTask(id) appelant DELETE /api/tasks/:id avec gestion d'erreur du style existant
- [ ] 2. board.js affiche un bouton delete par task, gated par une confirmation explicite
- [ ] 3. annuler la confirmation ne fait AUCUN appel réseau
- [ ] 4. delete réussi retire la task de la vue sans reload complet ou avec refetch — comportement décrit dans le done report
- [ ] 5. go build ./... vert dans le worktree; v1 (static/index.html) zéro diff

## 2. Root cause & decisions

# S2 — delete task button in v2 console (task 654d358d, [split])

ROOT_CAUSE: The v2 console had no way to delete a task even though the server
already exposed DELETE /api/tasks/:id (apiDeleteTask, api.go:1838; TestAPIDeleteTask).
The capability existed backend-side but was unreachable from the operator UI.

## Decision
Expose delete in the task-detail slide-over command section (already home to the
destructive/orchestrator ops reassign + force-status, already gated to native
editable tasks). Per-card delete rejected (misclick risk, board noise).

Per RULING S2 (wraith-cto, option A): ticket [split], add ONE Go test file with
5 subtests mapped 1:1 to the 5 ACs (the reviewer treats the plan test names as
the contract, so they must exist).

## Implementation (4 files)
- api.js: deleteTask(id, project) — sendJSON('DELETE', /api/tasks/:id?project=…),
  same error style as deleteMemory/deleteRule.
- board.js: "danger" delete button in commandSection; wireCommand handler gates
  the fetch with window.confirm (cancel returns before any request), then closes
  the sheet and removes the card via reconcile (no full reload).
- v2.css: .cmd-btn.danger red styling (mirrors .mem-act.danger).
- internal/relay/console_v2_delete_test.go: TestV2DeleteTask, 5 subtests.

## Rejected alternatives
- Per-card delete button: misclick risk, board noise.
- No test: overruled by RULING S2 — 5 real subtests required.

## Verification
- go build -tags fts5 ./... green; go vet -tags fts5 clean; gofmt clean.
- go test -tags fts5 ./internal/relay/ ok; TestV2DeleteTask 5/5 subtests PASS
  (WrapperDeleteTaskPresent, ButtonGatedByConfirm, CancelNoNetwork,
  DeleteRemovesTaskLive, BuildGreenV1ZeroDiff).
- Live relay round-trip: create -> board=1; DELETE {deleted:true} -> board=0.
- v1 showroom (static/index.html, static/js/) zero diff (git diff --stat main…HEAD
  lists only the 4 files above).

## Known limitation
CancelNoNetwork is a source-level guarantee (exactly one deleteTask call site,
behind a negated early-return). A true DOM/runtime click test needs a JS harness;
none exists in the repo.

## review-wraith verdict: SHIP
Scope: internal/web/static/v2/{api.js,board.js,v2.css} + internal/relay/console_v2_delete_test.go — v2 console delete-task button + its Go test
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (TestV2DeleteTask 5/5 + full relay pkg green)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- CancelNoNetwork is source-level only (no JS DOM harness in repo) — stated as a known limitation.

## 3. Files changed

```
internal/relay/console_v2_delete_test.go | 123 +++++++++++++++++++++++++++++++
 internal/web/static/v2/api.js            |   2 +
 internal/web/static/v2/board.js          |  17 +++++
 internal/web/static/v2/v2.css            |   2 +
 4 files changed, 144 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `654d358d-13c8-431a-99f7-1f60334fe5d6`._
