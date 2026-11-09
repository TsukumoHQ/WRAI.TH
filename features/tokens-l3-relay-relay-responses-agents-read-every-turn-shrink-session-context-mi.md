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

## Round 2 (QA r1 REJECTED on AC4)
ROOT_CAUSE: sessionFloorCeiling was checked against the shared running `used`; constraints come first in ListBootMemories, so fat constraints spent the ceiling and the behavior/context floors (af783f93) dropped (reviewer repro: constraints:6 vs main behavior:1 constraints:8 context:1).
DECISION: pre-scan reserves the bytes of the behavior/context floor rows; a floor row is admitted while used+b+reserved <= sessionFloorCeiling, each reserved row releasing its own reservation when met. Constraints still bounded (AC4), other floors guaranteed. Test TestProjectMemories_FloorCeilingKeepsBehaviorContextFloors; mutation (drop +reserved) fails it.

RED_EVIDENCE:
  cmd: go test ./internal/relay/ -run TestProjectMemories_FloorCeilingKeepsBehaviorContextFloors   (new test from 5705725 run against project.go of 6efda31, pre-fix)
  test_sha: 5705725
  output: |
    --- FAIL: TestProjectMemories_FloorCeilingKeepsBehaviorContextFloors (0.00s)
        compact_echo_test.go:379: behavior/context floors dropped by fat constraints: layers map[constraints:6]
    FAIL
    FAIL	agent-relay/internal/relay	0.552s

## 3. Files changed

```
...ts-read-every-turn-shrink-session-context-mi.md |  66 ++++
 internal/relay/compact_echo_test.go                | 387 +++++++++++++++++++++
 internal/relay/handlers_agents.go                  |  19 +-
 internal/relay/handlers_tasks.go                   |  16 +-
 internal/relay/handlers_test.go                    |   8 +-
 internal/relay/project.go                          |  35 ++
 internal/relay/self_grading_guard_test.go          |   4 +-
 internal/relay/task_echo.go                        | 127 +++++++
 internal/relay/tools.go                            |   9 +-
 internal/relay/toolset.go                          | 154 +++++++-
 internal/relay/toolset_test.go                     |  14 +-
 internal/relay/typed_ticket_test.go                |   4 +-
 internal/relay/update_task_contract_test.go        |   2 +-
 internal/relay/update_task_typed_fields_test.go    |   2 +-
 internal/relay/verify_cmd_test.go                  |   2 +-
 skill/tools-reference.md                           |   3 +
 16 files changed, 825 insertions(+), 27 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-ffd35635-8d53-4cb2-b8d8-519b93985d85
- 🟢 AC1: Each of the three tools returns id/status/profile_slug/assigned_to plus the relay-decided or call-set fields, and verbose=true restores the full task; verified by execution, not by reading. — evidence: internal/relay/task_echo.go:29-127 (new taskEchoBase/dispatchEcho/updateEcho), internal/relay/handlers_tasks.go:208-212 (dispatch), 1290-1294 (cancel), 1477-1481 (update); verboseParam added to all three tool schemas at internal/relay/tools.go:496,725,904. Executed the new tests: dispatch_task echo verbose=1182B compact=197B, update_task verbose=1171B compact=154B. — test: TestCompactEcho_Dispatch (internal/relay/compact_echo_test.go:30), TestCompactEcho_Update (:70), TestCompactEcho_Cancel (:113) - all behavioural (they read the MCP result content and the persisted row via get_task). Mutation-checked: reverting the three handlers_tasks.go hunks makes all three FAIL.
- 🟢 AC2: Re-register of a live agent defaults to minimal, first boot is byte-identical to before, and the two escape hatches (explicit full, comeback from inactive) are both asserted. — evidence: internal/relay/handlers_agents.go:141-142 (prior row read, wasLive = status active|sleeping, matching db.SenderEligibility at internal/db/agents.go:41) and :200-204 (ctxMode default empty; minimal when omitted and wasLive; mode note). Executed: first boot full, second register of the same live agent minimal with is_respawn=true and the mode note, explicit session_context=full stays full, register after DeactivateAgent boots full. — test: TestRegister_ReRegisterOfLiveAgentDefaultsMinimal (internal/relay/compact_echo_test.go:136) - behavioural, it drives HandleRegisterAgent and inspects the returned session_context. Mutation-checked: reverting the wasLive logic makes it FAIL.
- 🟢 AC3: Per-category reduction verified by execution, and an independent audit of the compact payload found no lost param and no kept as/project param. — evidence: internal/relay/toolset.go:570-660 (compact branch, compactTool, paramSignature) and tools.go:547 (verbose flag on discover_tools). Executed the new test, which logged every category: session -87%, messaging -42%, conversations -51%, tasks -48%, obligations -44%, pipeline -44%, boards -51%, memory -45%, profiles -42%, agents -44%, teams -56%, projects -82% - all >= 40%. The verbose branch returns the registered JSON Schemas verbatim, so schemas stay valid. — test: TestDiscoverCompact_SizeAndLossless (internal/relay/compact_echo_test.go:186) - behavioural, it measures both payloads per category and cross-checks every param name/type/required/enum against the verbose schema. TestDiscoverSchemasValidJSON (internal/relay/toolset_test.go:161) still validates the JSON Schema via verbose=true. Mutation-checked: reverting the compact branch makes the new test FAIL.
- 🔴 AC4: [partial] Constraints now obey the byte ceiling as intended, but the ceiling is evaluated against the shared running used counter instead of a per-floor counter, so a constraint-heavy project silently loses the behavior and context memory floors that project.go:74-80 documents as guaranteed. The criterion is met for its own subject but the shipped implementation breaks a sibling invariant, so it is not fully green. — evidence: internal/relay/project.go:69-72 (sessionFloorCeiling = 5200) and :707-712 (the new floor check). The constraint-counting behaviour the AC asks for does hold and is pinned, but the implementation of it is defective: executing a repro against the branch surfaces map[constraints:6] (behavior and context floors dropped) where origin/main surfaces map[behavior:1 constraints:8 context:1]. — test: TestProjectMemories_ConstraintsCountInBudget (internal/relay/compact_echo_test.go:284) - behavioural, and mutation-checked (reverting the project.go hunk makes it FAIL), but it only asserts the constraint ceiling and never checks that the behavior/context floors still surface, so it cannot catch the regression.

## 5. Timeline

- round 1 → **reject** (review-ffd35635-8d53-4cb2-b8d8-519b93985d85)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ffd35635-8d53-4cb2-b8d8-519b93985d85`._
