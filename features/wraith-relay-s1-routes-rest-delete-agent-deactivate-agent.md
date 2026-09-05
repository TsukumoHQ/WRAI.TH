# [wraith/relay] S1: routes REST delete_agent + deactivate_agent

## Team : wraith-backend (tsukumo)
## Branch : wraith/agent-delete-rest (from main)
## Relay task : 370a2438-5635-4d55-a1a5-b6a8dc73cbed
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. DELETE /api/agents/:name et POST /api/agents/:name/deactivate routés vers les handlers existants
- [ ] 2. même chaîne middleware/auth que les routes REST voisines (prouvé par le wiring, cité dans le report)
- [ ] 3. agent inconnu -> 404 avec l'enveloppe d'erreur standard (test)
- [ ] 4. test par route: succès + cas erreur
- [ ] 5. go test -tags fts5 ./internal/relay/... vert dans le worktree

## 2. Root cause & decisions

ROOT_CAUSE: delete_agent and deactivate_agent existed only as MCP tools (handlers_agents.go HandleDeleteAgent:357 / HandleDeactivateAgent:338) with no REST route in ServeAPI (audit b983684b §1). The v2 console can only call REST, so no delete-agent/deactivate-agent button could exist.

DECISION: add two REST routes to ServeAPI mirroring the existing agent-op wiring (same CORS -> RateLimit -> BodyLimit -> Auth middleware chain, same projectFromRequest resolution) — DELETE /api/agents/:name and POST /api/agents/:name/deactivate. Each wrapper reuses the SAME DB call the MCP handler uses (DB.DeleteAgent / DB.DeactivateAgent) plus Handlers.cascadeDeactivatedAgent (the Phase-2 referential-integrity soft-cascade) — zero new business logic. A GetAgent lookup first gives a clean 404 with the standard {"error":...} envelope for an unknown name.

REJECTED ALTERNATIVES:
- Skipping the GetAgent pre-check and relying on the UPDATE's WHERE clause to no-op an unknown agent: rejected because it returns 200 for a name that never existed, giving the console no way to distinguish success from typo; the AC explicitly requires a 404 for unknown agents.
- Factoring the MCP handler and REST wrapper into one shared private helper: rejected as premature — the two entry points differ in argument source (mcp.CallToolRequest vs http.Request) and result shape (tool JSON vs REST JSON); the shared surface is exactly the two DB calls + cascade, already reused directly. A wrapper helper would add indirection for two call sites, against the "no new business logic" constraint.

No LEGACY_OPPORTUNITY items surfaced — additive REST wiring over existing handlers, no refactor.

## review-wraith verdict: SHIP
Scope: internal/relay/api.go (2 routes in ServeAPI switch + apiDeactivateAgent/apiDeleteAgent wrappers), internal/relay/api_agents_test.go (new, 2 route tests).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt clean / test -tags fts5 OK (695 passed, 12 packages, full repo)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none. Routes sit behind the same middleware chain as every /api sibling (no auth bypass); reuse the existing DB ops + Phase-2 soft-cascade verbatim (no new writer, no new business logic, no schema change); DeleteAgent stays the deliberate tombstone (status='deleted', reversible), not a hard row delete; unknown agent 404s via GetAgent (RO-pool read).

## 3. Files changed

```
internal/relay/api.go             | 58 ++++++++++++++++++++++++++++++
 internal/relay/api_agents_test.go | 74 +++++++++++++++++++++++++++++++++++++++
 2 files changed, 132 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `370a2438-5635-4d55-a1a5-b6a8dc73cbed`._
