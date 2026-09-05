# [wraith/relay] HOTFIX: TestToolSchemaBudget rouge sur main (57393B > 57344, +49B) — trim descriptions tools.go

## Team : wraith-backend (tsukumo)
## Branch : wraith/schema-budget-hotfix (from main)
## Relay task : 414028d8-bbaa-4704-beba-6ab0ef822ee1
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. go test -tags fts5 ./internal/relay/ vert; TestToolSchemaBudget log total ≤ 57000 bytes (chiffre cité dans le PR)
- [ ] 2. diff = internal/relay/tools.go seul; uniquement des chaînes de description modifiées — zéro tool/param/required/type ajouté ou retiré (git diff cité dans le PR)
- [ ] 3. aucune description vidée: chaque outil garde une phrase d'intention + contraintes clés (jamais de reassign silencieux, refus loud, etc.)
- [ ] 4. go build -tags fts5 ./... vert; go test -tags fts5 ./... total vert (chiffre cité)

## 2. Root cause & decisions

ROOT_CAUSE: main went red on TestToolSchemaBudget because S3a (9cb7e6d) added the archive_project/unarchive_project tool verbs (tools.go +39/-19), pushing the serialized tool-schema total to 57393B over the 57344B budget (+49B). A scoped S3a verify (`./internal/db` only) and an empty postmerge check let the regression through, and every relay qa-submit then bounced on the red base.

DECISION: trim description STRINGS ONLY across the fattest tools to bring the total to 56963B (largest tool 2088B < 2300 cap) — 381B under the 57344 cap and below the 57000 target so the next verbs have headroom. Zero tool/param/required/type added or removed; no description emptied (each keeps its intent sentence + key constraints, e.g. "never reassign silently", "refuse loudly", broadcast admin-gate). Budget cap NOT raised — the sprint already moved it 48000→57344; the fix is trimming fat, not buying more.

REJECTED ALTERNATIVES:
- Raise toolSchemaBudgetBytes: masks description bloat, defeats the guard's purpose; cto ruled trim, not raise.
- Trim inside the S3b archived-guard diff: scope creep on an unrelated ticket; cto ruled (b) separate hotfix.
- Drop a whole tool description: violates AC ("aucune description vidée"); loses agent-facing intent.

```
## review-wraith verdict: SHIP
Scope: internal/relay/tools.go — description-string trims only (register_agent, send_message, dispatch_task, update_task, set_memory, send_status, list_tasks, get_inbox, remember). 20 insertions / 20 deletions, all on mcp.Description lines (git diff verified: zero non-Description changed lines).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK (no drift) / test -tags fts5 ./... OK (729 passed, 12 packages)

BLOCKERS (must fix before merge):
- none. No sqlite/schema/writer/deliveries/messaging/MCP-surface change. Tool set unchanged (76 tools; TestGuardInstalledOnMutatingTools + TestDiscoveryPairBudget green → registry + progressive disclosure intact). Backbone untouched.

NITS (non-blocking):
- none. Single-file, single-lane, additive-neutral; cannot red trunk. Staged explicit path (internal/relay/tools.go); commit trailer present; branch off main.
```

## 3. Files changed

```
internal/relay/tools.go | 40 ++++++++++++++++++++--------------------
 1 file changed, 20 insertions(+), 20 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `414028d8-bbaa-4704-beba-6ab0ef822ee1`._
