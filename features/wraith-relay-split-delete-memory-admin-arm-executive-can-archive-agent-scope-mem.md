# [wraith/relay][split] delete_memory admin arm: executive can archive AGENT-scope memories of dead/other agents + purge 5 orphan checkpoints

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/mem-admin (from main)
## Relay task : 9e1ab06b-cc3f-4862-bc8f-66623d0f15df
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: executive caller + `agent` param archives an agent-scope memory authored by an inactive agent; tombstone records both acting agent and target author
- [ ] 2. AC2 named test: non-executive caller targeting a LIVE other agent is refused with a message naming caller and target
- [ ] 3. AC3 the 5 niwa orphan keys (frontend-lead-resume, CKPT-frontend-lead-boot, DEC-dev-shutdown-checkpoint, CKPT-dev-3-shutdown, DEV-checkpoint) are archived post-merge, verified absent from list_memories
- [ ] 4. AC4 unknown-arg silent-drop trap avoided: `agent` param is in the schema (cf. wraith-archive-tasks-no-ids-param gotcha)

## 2. Root cause & decisions

# 9e1ab06b — delete_memory admin arm (founder memory-hygiene order)

## ROOT_CAUSE
`DeleteMemory(project, agentName, key, scope)` bound the agent-scope WHERE to
`agent_name = agentName` where `agentName` is the CALLER. So the delete could
only ever reach the caller's OWN agent-scope rows. An executive (or a janitor)
had no way to archive agent-scope garbage authored by a departed agent — live
symptom: 5 orphan shutdown/boot checkpoints in project niwa (frontend-lead-resume,
CKPT-frontend-lead-boot, DEC-dev-shutdown-checkpoint, CKPT-dev-3-shutdown,
DEV-checkpoint; authors frontend-lead/dev/dev-3/anonymous, all gone) polluting
every list_memories with no reachable delete path.

## FIX (3 source files + 1 test, ≤3 non-test src)
- internal/db/memories.go: split `DeleteMemory` into a back-compat shim over new
  `DeleteMemoryAs(project, actingAgent, targetAuthor, key, scope, reason...)`.
  actingAgent → archived_by (WHO reaped); for agent scope targetAuthor → the
  agent_name matched (WHOSE memory). So the tombstone records BOTH sides; the
  original author (agent_name) is preserved, archived_by names the reaper.
  Existing self-delete = DeleteMemory shim (targetAuthor==caller), byte-identical.
- internal/relay/handlers_memory.go: read optional `agent` target param. Default =
  caller (ordinary self-delete). A cross-author target is gated: allowed iff the
  caller is_executive OR the target author is dead (targetAuthorIsDead: GetAgent
  nil — never registered, e.g. "anonymous" — or status inactive/deleted). A
  non-executive aimed at a LIVE (active/sleeping) other agent is refused LOUD
  (CodeForbidden) naming caller AND target. `agent` on project/global scope (no
  agent_name dimension) refuses loudly rather than silently no-op.
- internal/relay/tools.go: `agent` added to the delete_memory schema (AC4 — avoids
  the unknown-arg silent-drop trap, cf. wraith-archive-tasks-no-ids-param).

## AC3 (post-merge, deferred)
The 5 niwa orphan keys are archived AFTER merge via delete_memory with
scope=agent + agent=<author> (each author is dead → the janitor branch permits it,
no exec needed). Requires the merged binary live on the running relay — coordinate
the relay update with CTO, then purge + list_memories check, then complete_task.

## VERIFY
- go build -tags fts5 ./... OK; go vet -tags fts5 clean; gofmt clean
- go test -tags fts5 ./... — all packages green
- TestToolSchemaBudget green, 51917 bytes (was 51647; +270 for the new param) < cap 57344
- No schema/migration; DeleteMemoryAs is a guarded UPDATE via the single writer
- AC1 TestDeleteMemory_ExecutiveArchivesDeadAgentMemory (tombstone records archived_by=chief + agent_name=ghost)
- AC2 TestDeleteMemory_NonExecCannotTargetLiveAgent (refusal names peer + victim; memory untouched)
- extra TestDeleteMemory_JanitorArchivesDeadAndUnregisteredAuthor (non-exec purges inactive + never-registered authors — the AC3 mechanism)

## review-wraith verdict: SHIP
Scope: internal/db/memories.go (DeleteMemory shim + DeleteMemoryAs), internal/relay/handlers_memory.go (agent target param + executive/dead-target authz gate + targetAuthorIsDead), internal/relay/tools.go (schema param), internal/relay/delete_memory_admin_test.go (new, 3 tests)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (all packages green; TestToolSchemaBudget green @ 51917 B < 57344)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none. No schema/migration (agentColumns↔scanAgent untouched). DeleteMemory shim keeps the old signature + behaviour byte-identical (existing memories_validity_test still green). New writer path is the existing writerExec guarded UPDATE (RowsAffected checked), not a new/hot writer. Authz is fail-closed + loud (default self, cross-author needs exec-or-dead-target, refusal names both). No inbox/SSE/auth-middleware/updater surface touched. api.go apiDeleteMemory uses DeleteMemoryByID (unrelated) — untouched. Single-lane, cannot red trunk. AC3 purge is post-merge by design (needs the live relay updated).

## Round 2 — reviewer findings addressed
1. FIXED (real bug): mixed-case `agent` param silently mismatched the lowercase-
   stored agent_name in the UPDATE — authz folds case (targetAuthorIsDead → GetAgent
   lowercased) so the permission check passed, but the store WHERE used the raw case
   and archived nothing. Fix: fold targetAuthor to lowercase at resolution
   (handlers_memory.go), so a param like "Frontend-Lead" resolves to its row.
   Regression test: TestDeleteMemory_MixedCaseTargetResolves (agent:"GHOST" archives
   the "ghost" row).
2. AC3 (purge 5 niwa keys) — deferral ACCEPTED BY LEAD: wraith-cto msg 7972dd68
   (03:24:55Z) explicitly agreed the sequence: gate-merge → cto-tsukumo redeploy #11
   → doer purges the 5 keys + list_memories check + complete_task with sha. The purge
   cannot run pre-merge: redeploy #10 (live) lacks the `agent` param, so the running
   relay would silent-drop it. Proof will land at complete_task post-redeploy.

Re-verified: go build/vet -tags fts5 clean; gofmt clean; go test -tags fts5 ./... all
packages green; 4 delete_memory tests pass (incl. mixed-case). AC1/AC2/AC4 green;
AC3 deferred with lead sign-off above.

## review-wraith verdict (round 2): SHIP
Same scope + one-line fold fix in handlers_memory.go + one regression test. No schema/
migration, backward-compat shim intact, budget unchanged. AC3 deferral lead-accepted.

## 3. Files changed

```
...in-arm-executive-can-archive-agent-scope-mem.md |  90 +++++++++++++
 internal/db/memories.go                            |  21 ++-
 internal/relay/delete_memory_admin_test.go         | 150 +++++++++++++++++++++
 internal/relay/handlers_memory.go                  |  66 ++++++++-
 internal/relay/tools.go                            |   1 +
 5 files changed, 319 insertions(+), 9 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-9e1ab06b-cc3f-4862-bc8f-66623d0f15df @ `cb1376078`
- 🟢 AC1: tombstone records both sides — evidence: handlers_memory.go:337-360 + db/memories.go:454-487 (archived_by=actingAgent, agent_name=targetAuthor); ran the test — test: TestDeleteMemory_ExecutiveArchivesDeadAgentMemory internal/relay/delete_memory_admin_test.go:41 - passes; reverting DeleteMemoryAs makes the UPDATE match agent_name=chief -> 'memory not found'
- 🟢 AC2: fail-closed loud refusal — evidence: handlers_memory.go:352-358 CodeForbidden naming caller+target; memory untouched asserted — test: TestDeleteMemory_NonExecCannotTargetLiveAgent internal/relay/delete_memory_admin_test.go:74 - passes
- 🔴 AC3: [partial] purge + verification outstanding; lead must accept the deferral explicitly or the doer must land the proof — evidence: nothing in the diff archives the 5 niwa keys; no list_memories absence check - deferred post-merge in the plan — test: TestDeleteMemory_JanitorArchivesDeadAndUnregisteredAuthor delete_memory_admin_test.go:100 covers only the MECHANISM (dead/never-registered author), not the purge itself
- 🟢 AC4: param in schema, no silent drop — evidence: internal/relay/tools.go:393 mcp.WithString("agent", ...) in deleteMemoryTool — test: TestToolSchemaBudget (pre-existing, green in the run: 738 pass)

## 5. Timeline

- round 1 → **reject** (review-9e1ab06b-cc3f-4862-bc8f-66623d0f15df)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `9e1ab06b-cc3f-4862-bc8f-66623d0f15df`._
