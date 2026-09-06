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
internal/relay/routing_param_types_test.go | 138 +++++++++++++++++++++++++++++
 internal/relay/toolset.go                  |  39 ++++++++
 2 files changed, 177 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `2540e596-354b-459a-9125-592301f9b8b8`._
