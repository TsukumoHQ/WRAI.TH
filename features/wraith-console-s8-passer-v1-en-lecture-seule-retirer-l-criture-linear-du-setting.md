# [wraith/console] S8: passer v1 en lecture seule — retirer l'écriture Linear du settings modal

## Team : wraith-engine (tsukumo)
## Branch : wraith/console-s8-v1-readonly (from main)
## Relay task : 48b0a6eb-f3fd-46e6-ba68-de29f9f7a9b5
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 AMENDÉ (wraith-cto 14:37Z, lecture NARROW): le modal Linear v1 ne peut plus écrire — bouton save, input clé, load-teams, radios team et l'appel updateSettings du save handler retirés; grep des clés d'écriture Linear (linear_enabled|linear_api_key|linear_team_key|linear_project|linear_interval|linear_routing|linear_project_map) dans static/ hors v2/ = zéro occurrence en écriture (lecture/affichage read-only autorisée). client.updateSettings() RESTE pour dyson_type (main.js:1650/1661/1675) — hors scope, cf. follow-up.
- [ ] 2. la config Linear reste visible en read-only (enabled/team/clé masquée) avec lien vers /v2/ (choix documenté dans le PR)
- [ ] 3. aucune régression sur le reste de la v1: pages chargées, listing OK, contrôles dyson toujours persistants — vérifié en serve local, étapes citées dans le corps du commit
- [ ] 4. v2 zéro diff
- [ ] 5. go build ./... vert dans le worktree; go test -tags fts5 ./... vert (chiffre cité)

## 2. Root cause & decisions

# S8 — v1 settings modal read-only (remove Linear config write)

ROOT_CAUSE: The v1 UI (internal/web/static/index.html + js/main.js) shipped a settings modal that could WRITE the Linear connector config — a save button plus client.updateSettings() (PUT /api/settings) with an API-key input and team selection (audit b983684b §6). This contradicts the founder "showroom" doctrine (2026-09-05): v1 must be a read-only vitrine, and config edits belong to the v2 console (S5). The write surface in a demo UI let a viewer mutate live Linear config.

DECISION (NARROW, per cto-tsukumo ruling 2026-09-05 14:37Z): remove the Linear config WRITE path from the v1 settings modal only.
- index.html: dropped the enabled checkbox, API-key input, "Charger les teams" button, team radios, and the "Enregistrer" save button; replaced with a read-only display (mode / team / masked key) plus a link to /v2/.
- js/main.js: dropped the set-linear-save handler (the updateSettings write), the load-teams handler, and the key-input write. openSettings now only GETs via fetchSettings and populates the read-only spans.
- client.updateSettings() is KEPT in api-client.js — the dyson_type visual controls (main.js:1650/1661/1675) still write it and must keep persisting (AC3). api-client.js untouched.
- v2/ and the backend /api/settings route are untouched; v1 simply stops calling the write path.

REJECTED ALTERNATIVES:
- BROAD (v1 zero config write at all): would require removing updateSettings() and re-plumbing dyson_type to localStorage/client-only. More files, regression risk on the galaxy visual persistence, and out of scope for audit §6 (Linear config write). Dyson write via v1 is tracked as a separate follow-up flagged to cto-tsukumo.
- Remove the modal entirely: the ruling preferred keeping a read-only view with a pointer to v2 over hiding config state from the operator.

[LEGACY_OPPORTUNITY] none — additive/subtractive UI change; no dead legacy path uncovered beyond the removed write handlers themselves.

## review-wraith verdict: SHIP
Scope: internal/web/static/index.html, internal/web/static/js/main.js (frontend static only; no Go/sqlite/schema/messaging/MCP/updater surface touched).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt N/A (no .go files changed) / test -tags fts5 OK (750 passed).

BLOCKERS (must fix before merge):
- none.

NITS (non-blocking):
- none. Change is subtractive (removes the Linear write handlers) + a read-only display; updateSettings() retained for dyson_type; api-client/v2/backend untouched; serve-local confirmed 0 write widgets, dyson×3 intact, v2 + GET /api/settings 200.

## 3. Files changed

```
internal/web/static/index.html | 23 +++++++++---------
 internal/web/static/js/main.js | 53 ++++++++----------------------------------
 2 files changed, 21 insertions(+), 55 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `48b0a6eb-f3fd-46e6-ba68-de29f9f7a9b5`._
