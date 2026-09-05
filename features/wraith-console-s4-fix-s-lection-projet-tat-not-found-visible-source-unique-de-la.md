# [wraith/console] S4: fix sélection projet — état 'not found' visible + source unique de la liste

## Team : wraith-engine (tsukumo)
## Branch : wraith/console-s4-project-select (from main)
## Relay task : a6c3e86f-4d87-4adb-86ef-729eaa1f1c7c
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. deep-link vers un projet inexistant affiche un état 'project not found' visible avec action de retour — plus de redirect silencieux
- [ ] 2. une seule source de vérité pour la liste projects (ctx partagé ou refetch on-focus — choix documenté dans le report)
- [ ] 3. créer/voir un projet depuis home le rend visible dans le sélecteur sans reload manuel
- [ ] 4. zéro régression sur la navigation projet existante (flows listés + vérifiés dans le report)
- [ ] 5. go build ./... vert; v1 zéro diff

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/web/static/v2/v2.js, internal/web/static/v2/home.js, internal/relay/console_v2_project_select_test.go (console v2 router + home page + served-asset regression test)
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (TestV2ProjectSelect 6/6)

ROOT_CAUSE: v2.js loaded the projects list once at boot (initHeader) and never refetched, while home.js kept its own copy refetched on activate — the two desynced. On an unknown-project deep link the router silently redirected home (location.hash='#/'), and a project created/seen on home was invisible to the router's stale list.

FIX: v2.js is now the single source of truth via ctx.refreshProjects() (refetch + update the shared array); home.js reads through it instead of ctx.api.projects(). The router refetches and rechecks before declaring a project unknown, then renders a visible "project not found" panel with a back-to-fleet action instead of a silent redirect.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- No JS DOM harness in-repo, so the 5 subtests assert the served v2 assets at the string level; the two behavioural flows (unknown deep-link, create-on-home-then-enter) were exercised manually against a local serve. Structural coverage is the documented limit.
- Not-found panel markup + styles are built in v2.js (router-owned) to keep the change within the 3-file plan cap; v1 (static/index.html) is byte-untouched.

## 3. Files changed

```
internal/relay/console_v2_project_select_test.go | 88 ++++++++++++++++++++++++
 internal/web/static/v2/home.js                   |  2 +-
 internal/web/static/v2/v2.js                     | 74 ++++++++++++++++++--
 3 files changed, 157 insertions(+), 7 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `a6c3e86f-4d87-4adb-86ef-729eaa1f1c7c`._
