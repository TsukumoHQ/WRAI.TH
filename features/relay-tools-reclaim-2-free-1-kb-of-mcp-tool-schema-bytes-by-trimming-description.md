# [relay/tools] reclaim #2: free >=1 KB of MCP tool-schema bytes by trimming descriptions (zero contract change)

## Team : wraith-engine (tsukumo)
## Branch : chore/tool-schema-reclaim-2 (from main)
## Relay task : cca54020-a066-4d74-b744-6980a85829ce
## Trace : trace=e01c9b86334afc3d15ad60136c928535
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestToolSchemaBudget green with constants unchanged and total schema bytes >= 1024 B lower than the branch point; PR body gives before/after totals and the 10 largest per-tool deltas.
- [ ] 2. AC2 zero contract change: a test (or its output in the PR) shows tool names and each tool's parameter names, types, enums and required lists byte-identical before/after (reuse the reclaim #1 comparison).
- [ ] 3. AC3 no description of resolve_conflict, claim_task, reclaim_task, guard or task_edge changes (git diff shows none).
- [ ] 4. AC4 go vet clean; go test -tags fts5 -race ./internal/relay/... green.

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/relay/tools.go (32 description strings only)
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -tags fts5 -race ./internal/relay/... OK (645 passed)

AC1: total tool schema 54714 -> 53386 B (-1328 B, >= 1024). TestToolSchemaBudget margin 3958 B, constants unchanged (57344 / 2048).
10 largest per-tool deltas:
  -153	register_agent	1892->1739
  -132	dispatch_task	1992->1860
  -102	send_message	2172->2070
  -87	set_run	830->743
  -87	whoami	478->391
  -72	create_project	672->600
  -65	batch_dispatch_tasks	719->654
  -65	register_profile	742->677
  -60	delete_team	513->453
  -54	deadletter	575->521
AC2 zero contract change: throwaway dump test (not committed, the reclaim #1 method): each tool marshalled with every "description" key stripped, sorted. Before and after files are byte-identical (sha1 b18507b7eae3 both): same tool names, and per tool the same param names, types, enums and required lists.
AC3: resolve_conflict 700->700, claim_task 613->613, reclaim_task 551->551, task_edge 647->647 (sizes unchanged, none of their lines in the diff). guard is not on main. Shared asParam/projectParam untouched, so the excluded tools stay byte-identical.
Kept in every rewrite: refusal/permission rules (dispatcher-only, executives-only, refused-if-several-boards, TASK rules), Irreversible on delete_project, no-wake semantics of send_status. Rewrites avoid < > & (json escapes each to 6 B).

BLOCKERS: none
NITS: none

ROOT_CAUSE: chore. The fixed 57344 B cap had 82 B usable after guard S2a; contradictions T2 (0090fabd) and release (545a11d2) need room.
DECISION: shorten the 32 longest non-excluded descriptions; no shared-param edits, so the excluded tools stay byte-identical.
REJECTED: trimming the shared asParam/projectParam descriptions (~1.2 KB across 82 tools, but it would change the excluded tools' bytes, breaking AC3); raising the cap (ruling ed744dee).

## 3. Files changed

```
internal/relay/tools.go | 64 ++++++++++++++++++++++++-------------------------
 1 file changed, 32 insertions(+), 32 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `cca54020-a066-4d74-b744-6980a85829ce`._
