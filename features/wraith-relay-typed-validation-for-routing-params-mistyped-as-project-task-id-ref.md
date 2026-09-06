# [wraith/relay] typed validation for routing params: mistyped as/project/task_id refuses — never silently coerces to default identity/project

## Team : wraith-backend (tsukumo)
## Branch : fix/routing-param-typed-validation (from main)
## Relay task : 2540e596-354b-459a-9125-592301f9b8b8
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: a tool call with `as` present as a non-string refuses INVALID_ARGUMENT naming 'as'; nothing executes (no row written, no message sent)
- [ ] 2. AC2 named test: `project` present as a non-string refuses identically — the call never resolves to the default project namespace
- [ ] 3. AC3 named test: absent as/project keep today's default-resolution behavior byte-identical (regression guard)
- [ ] 4. AC4 named test: task_id present as a non-string on a task tool refuses naming 'task_id' instead of a NOT_FOUND or empty-id lookup

## 2. Root cause & decisions

# Decision — task 2540e596 typed validation for routing params

ROOT_CAUSE: resolveAgent (handlers.go) and resolveProject (project_ns.go) read `as`/`project` via mcp GetString, and every task handler reads `task_id` the same way. GetString silently folds a present-but-non-string value (a native array, a number, a JSON null) to "". For these three routing params that "" then resolves to the DEFAULT identity/project (context or single-registration fallback) or an empty task lookup — so a mistyped `as`/`project` made the call EXECUTE under the wrong identity or in the wrong project namespace (silent misroute / potential cross-project write), and a mistyped `task_id` degraded into a NOT_FOUND. Same silent-coercion family as 88510cb5 (content fields) but with a worse blast radius.

FIX: a single shared seam. `guardRoutingParamTypes` wraps every toolRegistry handler OUTERMOST — applied after the guardIdentity loop so it runs BEFORE guardIdentity and any resolveProject/resolveAgent/resolveTaskID call. A present-but-non-string `as`/`project`/`task_id` is refused with INVALID_ARGUMENT (CategoryValidation, non-retryable) naming the param, before any write. An ABSENT param is untouched (given == false), so context/registration default-resolution is byte-identical to before. Applied to EVERY tool (reads route namespace/identity too); call_tool/discover_tools are registered separately and carry no routing params at their top level, and call_tool's inner dispatch runs through these same wrapped handlers, so nested calls are covered.

## Round 2 — AC4 test strengthened (reviewer finding, r1 REJECTED)
r1 marked AC1/AC2/AC3 green; AC4 partial. Finding: every task handler already
short-circuits an empty task_id ("task_id is required" → INVALID_ARGUMENT, no
lookup), so TestRoutingParams_TaskIDNonStringRefused passed on pre-fix code and
pinned nothing (rule (e)). Reviewer offered two fixes: strengthen the assertion,
or drop task_id from the guard as redundant.
Chosen: KEEP task_id + strengthen. task_id is an explicit ticket AC (AC4 names a
task_id test), and the guard is a genuine improvement — for a value that WAS
supplied but wrong-typed, "task_id is required" is a misleading error, the exact
silent-coercion family this ticket fixes; the guard reports the accurate
"task_id must be a string" uniformly at one seam (also covering any future
task_id-reading tool, not just today's 15 handlers). The test now asserts the
guard's distinctive "must be a string" message; revert-check confirmed: removing
task_id from routingParamKeys FAILS the test (routing_param_types_test.go:150).
No production code changed round 2 — test-only strengthening; 836 pass.

## review-wraith verdict: SHIP
Scope: internal/relay/toolset.go (guardRoutingParamTypes + wiring in toolRegistry), internal/relay/routing_param_types_test.go (4 tests)
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (836 passed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

Notes on the shared-backbone invariants: no DB write added (pure pre-dispatch type check) — single-writer + lock discipline untouched; no schema/migration change (agentColumns↔scanAgent untouched); no task-transition change (no TOCTOU surface); inbox stays non-destructive — AC1 asserts a refused send writes no row (recipient inbox empty); middleware order / auth untouched (guard sits at the tool-handler layer, after auth); MCP tool set still sourced from the single registry and does not bypass resolveProject/resolveAgent (it runs ahead of them and only refuses malformed input, else delegates).

## 3. Files changed

```
...uting-params-mistyped-as-project-task-id-ref.md |  52 +++++++
 internal/relay/routing_param_types_test.go         | 152 +++++++++++++++++++++
 internal/relay/toolset.go                          |  39 ++++++
 3 files changed, 243 insertions(+)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-2540e596-354b-459a-9125-592301f9b8b8 @ `840505940`
- 🟢 AC1: test exercises the registry-wrapped send_message handler with as:[]any{"sender"}; refusal message names as and carries CodeInvalidArgument; recipient inbox asserted empty (no row written) — evidence: internal/relay/toolset.go:230-232 wraps every handler with guardRoutingParamTypes; removing the guard wrap makes TestRoutingParams_AsNonStringRefusedNothingSent fail with INTERNAL instead of INVALID_ARGUMENT — test: TestRoutingParams_AsNonStringRefusedNothingSent internal/relay/routing_param_types_test.go:28
- 🟢 AC2: test sends project:42; without guard the call resolves to sender sole registration and executes; with guard refused INVALID_ARGUMENT naming project before any resolution — evidence: internal/relay/toolset.go:230-232 + project_ns.go:33 (resolveProject falls through to single-registration default when GetString folds project:42 to ""); removing the guard makes TestRoutingParams_ProjectNonStringRefused fail with success (silent misroute to default project) — test: TestRoutingParams_ProjectNonStringRefused internal/relay/routing_param_types_test.go:60
- 🟢 AC3: regression guard: absent keys untouched (given==false), valid string keys pass through; default-resolution byte-identical to pre-fix — evidence: internal/relay/toolset.go:248-261 guardRoutingParamTypes skips keys where given==false; TestRoutingParams_AbsentAndValidUnchanged asserts valid string params pass and absent project falls through to sender sole registration (2 messages delivered) — test: TestRoutingParams_AbsentAndValidUnchanged internal/relay/routing_param_types_test.go:84
- 🔴 AC4: [partial] AC behavior (refuse INVALID_ARGUMENT naming task_id, not NOT_FOUND, no DB lookup) was already met by pre-fix `if taskID == "" { return toolResultError("task_id is required") }` in every task handler (handlers_tasks.go:15 sites); fix adds a redundant guard whose specific "must be a string" message is not exercised by any test — per rule (e) a test green before the fix pins nothing — evidence: internal/relay/toolset.go:242 lists task_id in routingParamKeys; TestRoutingParams_TaskIDNonStringRefused passes even when task_id is removed from the list — the test pins the AC observable behavior but not the new code path — test: TestRoutingParams_TaskIDNonStringRefused internal/relay/routing_param_types_test.go:118

## 5. Timeline

- round 1 → **reject** (review-2540e596-354b-459a-9125-592301f9b8b8)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `2540e596-354b-459a-9125-592301f9b8b8`._
