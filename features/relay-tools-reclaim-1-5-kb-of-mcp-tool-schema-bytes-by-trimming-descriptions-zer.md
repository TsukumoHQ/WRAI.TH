# [relay/tools] reclaim >=1.5 KB of MCP tool-schema bytes by trimming descriptions (zero behavior change)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/schema-reclaim (from main)
## Relay task : edad85b0-a456-40f2-9226-21e76d22d927
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestToolSchemaBudget green with the constants unchanged, and total schema bytes reduced by >= 1536 B vs main at branch point; PR body gives before/after total and the 10 largest per-tool deltas.
- [ ] 2. AC2 zero contract change: a test (or go test run output in the PR) shows the set of tool names, and for each tool the parameter names, types, enums and required lists, are byte-identical before/after (e.g. marshal each tool's InputSchema with descriptions stripped and compare).
- [ ] 3. AC3 go test -tags fts5 -race ./internal/relay/... green (existing handler tests unaffected).

## 2. Root cause & decisions

ROOT_CAUSE: the fixed 57344 B tool-schema budget (sent to every agent on every boot) had ~131 B left over the 2048 B headroom after knowledge S2 + consumption T2. The descriptions carried examples, history and restated types that no agent needs to call a tool correctly. Coherence T2 was blocked.

DECISION (ruling ed744dee): reclaim, and keep the cap. Only the 27 longest descriptions in tools.go were rewritten. Kept: required fields, enum meanings, refusal and permission conditions (TASK_LEASE_HELD, executive/inactive rules, typed-ticket skips, Linear routing, the no-wake semantics of action_required). The rewrite also avoids <, > and & in rewritten text: Go's json.Marshal escapes each to 6 bytes (e.g. 'team:<slug>' became 'team:SLUG', '>1' became 'several'). toolset.go needed no change.

PROOF (AC1/AC2), from a throwaway dump test (not committed) run before and after:
- total schema bytes: 54596 -> 52690 (-1906 B, >= 1536). TestToolSchemaBudget margin 4654 with constants unchanged.
- zero contract change: each tool marshalled with every "description" key stripped, sorted. Before and after files are byte-identical (sha1 b90da450a88e3baf both): the same tool names, and per tool the same param names, types, enums and required lists.
- 10 largest per-tool deltas:
  -243	batch_dispatch_tasks	962->719
  -150	register_agent	2042->1892
  -115	identity_check	664->549
  -109	send_message	2281->2172
  -107	reconcile_pr	948->841
  -106	delete_memory	936->830
  -103	get_message	749->646
  -84	reclaim_task	635->551
  -80	get_run	498->418
  -77	create_project	749->672

## review-wraith verdict: SHIP
Scope: internal/relay/tools.go (descriptions only)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -count=1 -tags fts5 -race ./internal/relay/... OK (621 passed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- consumption T2 (in gate) adds who_consumed (+569 B). After both merge the margin is ~4085 B, which clears coherence T2 (~150 B), guards S2 (~500 B) and contradictions T2.

## 3. Files changed

```
internal/relay/tools.go | 54 ++++++++++++++++++++++++-------------------------
 1 file changed, 27 insertions(+), 27 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `edad85b0-a456-40f2-9226-21e76d22d927`._
