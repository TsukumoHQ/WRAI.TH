# [relay] update_task ignore goal/acceptance_criteria/dod — un re-scope ne peut pas mettre a jour le contrat du ticket

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/update-task-contract (from main)
## Relay task : c229fe44-b691-45e6-ae9a-1e40106dbec0
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. update_task par le dispatcher avec goal/AC/dod -> champs mis a jour, ancienne version conservee/consultable
- [ ] 2. update_task par l'assignee sur ces champs -> refus explicite
- [ ] 3. Aucun chemin ou ces champs sont ignores en silence
- [ ] 4. go test -tags fts5 ./... vert + qa-submit

## 2. Root cause & decisions

> ⚠️ Root cause / arbitration not recorded by the doer yet. The gate requires it before merge — this gap is visible on purpose.

## 3. Files changed

```
internal/db/tasks.go                        |  44 +++++++++-
 internal/relay/api.go                       |   2 +-
 internal/relay/handlers_tasks.go            |  34 +++++++-
 internal/relay/tools.go                     |   5 +-
 internal/relay/update_task_contract_test.go | 131 ++++++++++++++++++++++++++++
 5 files changed, 209 insertions(+), 7 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c229fe44-b691-45e6-ae9a-1e40106dbec0`._
