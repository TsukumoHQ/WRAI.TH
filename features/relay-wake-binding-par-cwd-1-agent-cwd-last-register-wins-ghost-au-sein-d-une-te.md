# [relay] wake binding par cwd: 1 agent/cwd, last-register-wins -> ghost au sein d'une team meme-cwd

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/cwd-wake-binding (from main)
## Relay task : 8362f5f2-bc41-4ee9-bd8a-ba4b370cbb54
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. Repro test: 2 agents registrent avec le meme cwd -> aucun ne devient ghost, identity_check bound_uniquely=true pour les deux (ou semantique equivalente)
- [ ] 2. Resolution wake par identite stable (session_id/agent-name) au lieu de cwd-exclusif, OU N agents par cwd avec desambiguation deterministe
- [ ] 3. Pas de regression: cwd ambigu sans autre signal continue de ne rien binder plutot que deviner
- [ ] 4. go test ./... vert + qa-submit

## 2. Root cause & decisions

> ⚠️ Root cause / arbitration not recorded by the doer yet. The gate requires it before merge — this gap is visible on purpose.

## 3. Files changed

```
internal/db/agents.go               | 82 +++++++++++++++++--------------------
 internal/db/agents_identity_test.go | 82 +++++++++++++++++++++++++++----------
 internal/relay/handlers_agents.go   | 30 +++++++-------
 internal/relay/tools.go             |  2 +-
 4 files changed, 112 insertions(+), 84 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `8362f5f2-bc41-4ee9-bd8a-ba4b370cbb54`._
