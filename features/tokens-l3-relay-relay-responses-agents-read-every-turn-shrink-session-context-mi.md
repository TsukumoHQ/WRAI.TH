# [tokens L3/relay] relay responses agents read every turn shrink: session_context minimal by default on re-register, discover_tools compact, mutation echoes trimmed

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/ffd35635-relay-compact (from main)
## Relay task : ffd35635-8d53-4cb2-b8d8-519b93985d85
## Trace : trace=ca70deebb0d7d42c24c017e0250b565b
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1: update_task / dispatch_task / cancel_task responses return a compact echo (id, status, changed fields) unless verbose=true; tests
- [ ] 2. AC2: re-register of an existing active agent returns minimal session_context by default; first boot unchanged; tests
- [ ] 3. AC3: discover_tools payload per category reduced >=40% (bytes before/after in PR body), schemas still valid
- [ ] 4. AC4: constraints memories counted in sessionMemoryBudget (test)

## 2. Root cause & decisions

ROOT_CAUSE: relay MCP results were ~26% of agent tool-result bytes (founder audit 2026-11-09): dispatch/update/cancel echoed the whole task (description, goal, AC, dod, a dozen timestamps) the caller had just written; register_agent rebuilt the full boot on every re-register of an already-running agent; discover_tools sent raw JSON Schemas with the shared as/project params repeated on every tool and re-sent the bootstrap tools already on tools/list; the boot memory layer floors had no byte bound.
DECISION: compact by default, verbose=true opt-out on the three mutating tools and discover_tools (get_task always returns the full record). update_task echoes `updated` field names, repeats only short values, adds lease fields on a reassign. register_agent defaults to minimal only when the row was live (active|sleeping) before the call — a first boot or a comeback from inactive still boots full; get_session_context default left 'full' (an agent always has a row when calling it, so a live-row rule would make full unreachable without the param; AC2 scopes register). discover_tools compact = one signature string per param "[*][type|enum][: desc]", lossless on name/type/required/enum (pinned against the verbose schema in TestDiscoverCompact_SizeAndLossless); bytes per category verbose->compact: session 3112->413, messaging 7662->4442, conversations 2629->1279, tasks 13820->7234, obligations 1579->889, pipeline 2935->1654, boards 1431->706, memory 7886->4312, profiles 1377->797, agents 2186->1234, teams 3313->1456, projects 1244->219. Layer floors bounded by sessionFloorCeiling = 2x sessionMemoryBudget, independent of the caller budget so the floor guarantee holds under a tiny budget.
REJECTED: compact JSON Schema (strip as/project, drop "type":"string", cap descriptions) — measured only 17-34% per category, the schema scaffolding itself is the floor. Floor bounded by the caller's hard ceiling — broke the floor guarantee at tiny budgets (TestProjectMemories_ConstraintFloorAndBudget_R1).
UNCERTAIN: AC3 "schemas still valid" read as: verbose=true returns the unchanged JSON Schemas; the compact default is a lossless signature form, not JSON Schema.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/relay (task_echo.go new, handlers_tasks.go, handlers_agents.go, toolset.go, tools.go, project.go) + tests + skill/tools-reference.md
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK

BLOCKERS (must fix before merge):
- none. No schema/migration change, no new write (register adds one RO GetAgent read before the existing write), agentColumns/scanAgent untouched, no task-transition logic touched, inbox untouched.

NITS (non-blocking):
- Agents that parsed description/goal/AC from a dispatch/update echo now need verbose=true or get_task (no in-repo or niwa consumer found; agent-hook.sh discards echoes).
- batch_dispatch_tasks still echoes full tasks — not in the ticket's AC; follow-up candidate.
- Relay redeploy is a separate CTO step after founder OK (DoD).
- handlers_test.go TestRecallCaptureEndToEnd: skip without FTS5 (pre-existing red on main under the task's tag-less verify_cmd, 2/3 runs).

## 3. Files changed

```
internal/relay/compact_echo_test.go             | 339 ++++++++++++++++++++++++
 internal/relay/handlers_agents.go               |  19 +-
 internal/relay/handlers_tasks.go                |  16 +-
 internal/relay/handlers_test.go                 |   8 +-
 internal/relay/project.go                       |  13 +
 internal/relay/self_grading_guard_test.go       |   4 +-
 internal/relay/task_echo.go                     | 127 +++++++++
 internal/relay/tools.go                         |   9 +-
 internal/relay/toolset.go                       | 154 ++++++++++-
 internal/relay/toolset_test.go                  |  14 +-
 internal/relay/typed_ticket_test.go             |   4 +-
 internal/relay/update_task_contract_test.go     |   2 +-
 internal/relay/update_task_typed_fields_test.go |   2 +-
 internal/relay/verify_cmd_test.go               |   2 +-
 skill/tools-reference.md                        |   3 +
 15 files changed, 689 insertions(+), 27 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ffd35635-8d53-4cb2-b8d8-519b93985d85`._
