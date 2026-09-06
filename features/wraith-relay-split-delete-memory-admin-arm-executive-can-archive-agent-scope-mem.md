# [wraith/relay][split] delete_memory admin arm: executive can archive AGENT-scope memories of dead/other agents + purge 5 orphan checkpoints

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/mem-admin (from main)
## Relay task : 9e1ab06b-cc3f-4862-bc8f-66623d0f15df
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: executive caller + `agent` param archives an agent-scope memory authored by an inactive agent; tombstone records both acting agent and target author
- [ ] 2. AC2 named test: non-executive caller targeting a LIVE other agent is refused with a message naming caller and target
- [ ] 3. AC3 named test: the dead-author path archives an agent-scope memory whose author is unregistered/inactive WITHOUT an executive caller (janitor case), tombstone preserving the original author
- [ ] 4. AC4 unknown-arg silent-drop trap avoided: `agent` param is in the schema (cf. wraith-archive-tasks-no-ids-param gotcha)

## 2. Root cause & decisions

ROOT_CAUSE: delete_memory resolved scope=agent against the CALLER's own agent row only — agent_name in the archival UPDATE was hard-bound to the caller. So an executive (or the fleet janitor) could never archive agent-scope garbage authored by a dead/other agent; observed live as 5 orphan niwa checkpoints that no one could clear.

DECISION: split the store call into DeleteMemoryAs(actingAgent, targetAuthor): archived_by records WHO reaped, agent_name (targetAuthor) selects WHOSE row. Add an optional `agent` target param on delete_memory, honored ONLY for scope=agent and ONLY when caller.IsExecutive OR the target author is dead (targetAuthorIsDead: GetAgent nil/err, or status inactive/deleted). Non-exec aimed at a LIVE agent is refused loudly naming both. Caller-supplied target is case-folded to the lowercase-stored agent_name so the authz walk (which folds case) can't pass while the UPDATE matches nothing. Self-delete path (targetAuthor==caller) unchanged — fully backward-compatible.

REJECTED ALTERNATIVES: (a) executive-only gate — rejected: the janitor purge of unregistered/anonymous checkpoint authors needs no exec rights once the author is provably dead; (b) a one-shot migration purging the 5 keys in-code — rejected: hard-coded operational data in a schema/handler diff; the live purge is DoD post-merge, not an in-diff AC (dispatcher amended AC3 03:37Z to the diff-verifiable janitor-path test).

## review-wraith verdict: SHIP
Scope: internal/relay/handlers_memory.go (delete_memory admin/janitor arm + targetAuthorIsDead gate), internal/db/memories.go (DeleteMemoryAs tombstone split), internal/relay/tools.go (`agent` param), internal/relay/delete_memory_admin_test.go (4 named tests)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (818 passed, 12 pkgs)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/memories.go:DeleteMemoryAs — no RowsAffected check; a target with a wrong key/author returns success while archiving nothing. Pre-existing DeleteMemory pattern; the realistic silent-no-op (case drift between authz walk and UPDATE) is already closed by the strings.ToLower fold on targetAuthor. Left as-is to avoid changing self-delete idempotency semantics.

Contract mapping:
- AC1 executive archives dead-agent agent-scope mem, tombstone records acting+target → TestDeleteMemory_ExecutiveArchivesDeadAgentMemory
- AC2 non-exec vs LIVE target refused naming both → TestDeleteMemory_NonExecCannotTargetLiveAgent
- AC3 (amended 03:37Z: janitor path) non-exec archives dead+unregistered author, tombstone preserves author → TestDeleteMemory_JanitorArchivesDeadAndUnregisteredAuthor
- AC4 `agent` param in schema, budget test green → tools.go + TestToolSchemaBudget
Invariants: single-writer intact, no schema/migration, no agentColumns/scanAgent touch, backward-compatible (self-delete unchanged), failure-is-loud.

## 3. Files changed

```
...in-arm-executive-can-archive-agent-scope-mem.md |  72 ++++++++++
 internal/db/memories.go                            |  21 ++-
 internal/relay/delete_memory_admin_test.go         | 150 +++++++++++++++++++++
 internal/relay/handlers_memory.go                  |  66 ++++++++-
 internal/relay/tools.go                            |   1 +
 5 files changed, 301 insertions(+), 9 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-9e1ab06b-cc3f-4862-bc8f-66623d0f15df @ `cb1376078`
- 🟢 AC1: tombstone records both sides — evidence: handlers_memory.go:337-360 + db/memories.go:454-487 (archived_by=actingAgent, agent_name=targetAuthor); ran the test — test: TestDeleteMemory_ExecutiveArchivesDeadAgentMemory internal/relay/delete_memory_admin_test.go:41 - passes; reverting DeleteMemoryAs makes the UPDATE match agent_name=chief -> 'memory not found'
- 🟢 AC2: fail-closed loud refusal — evidence: handlers_memory.go:352-358 CodeForbidden naming caller+target; memory untouched asserted — test: TestDeleteMemory_NonExecCannotTargetLiveAgent internal/relay/delete_memory_admin_test.go:74 - passes
- 🔴 AC3: [partial] purge + verification outstanding; lead must accept the deferral explicitly or the doer must land the proof — evidence: nothing in the diff archives the 5 niwa keys; no list_memories absence check - deferred post-merge in the plan — test: TestDeleteMemory_JanitorArchivesDeadAndUnregisteredAuthor delete_memory_admin_test.go:100 covers only the MECHANISM (dead/never-registered author), not the purge itself
- 🟢 AC4: param in schema, no silent drop — evidence: internal/relay/tools.go:393 mcp.WithString("agent", ...) in deleteMemoryTool — test: TestToolSchemaBudget (pre-existing, green in the run: 738 pass)

### Round 2 — ❌ REJECTED by review-9e1ab06b-cc3f-4862-bc8f-66623d0f15df @ `7f9c19a36`
- 🟢 AC1: tombstone helper reads actual DB row; not mock-based — evidence: internal/relay/handlers_memory.go:330-396 adds agent param + DeleteMemoryAs archival with acting/target split — test: TestDeleteMemory_ExecutiveArchivesDeadAgentMemory internal/relay/delete_memory_admin_test.go:44 (asserts archived_by=chief + agent_name=ghost via real DB row)
- 🟢 AC2: refusal verified with real state, not mock — evidence: internal/relay/handlers_memory.go:374 refusal msg uses caller(%s) + target(%s); fires when callerIsExec=false AND targetAuthorIsDead=false — test: TestDeleteMemory_NonExecCannotTargetLiveAgent internal/relay/delete_memory_admin_test.go:100 (checks peer+victim in msg + status unchanged)
- 🔴 AC3: [partial] AC literally says archived post-merge — actual archival of the 5 keys is operational follow-up not in this diff; plan approves this deferral. Mechanism verified, but the literal AC state is not. Reviewer should note for the next round that the post-merge purge still needs to be confirmed done. — evidence: diff adds the mechanism (targetAuthorIsDead helper + handler gate) but does NOT archive the 5 specific keys (frontend-lead-resume, CKPT-frontend-lead-boot, DEC-dev-shutdown-checkpoint, CKPT-dev-3-shutdown, DEV-checkpoint) — test: TestDeleteMemory_JanitorArchivesDeadAndUnregisteredAuthor internal/relay/delete_memory_admin_test.go:125 verifies the mechanism (non-exec can purge dead author + unregistered author)
- 🟢 AC4: param is wired through schema and exercised behaviorally — evidence: internal/relay/tools.go:393 adds mcp.WithString(agent, ...) to deleteMemoryTool — test: TestToolSchemaBudget internal/relay/toolsize_test.go:29 passes with the new param; 3 AC tests exercise the param reaching the handler end-to-end

### Round 3 — ✅ APPROVED by review-9e1ab06b-cc3f-4862-bc8f-66623d0f15df @ `a7ff8754f`
- 🟢 AC1: executive archives dead-agent agent-scope memory; tombstone records both sides end-to-end — evidence: handlers_memory.go:330-396 adds `agent` target param + admin gate; db/memories.go:454-487 splits DeleteMemoryAs(actingAgent, targetAuthor) — actingAgent→archived_by, targetAuthor→agent_name dimension — test: TestDeleteMemory_ExecutiveArchivesDeadAgentMemory internal/relay/delete_memory_admin_test.go:44 — PASSED; asserts archived_by=chief, agent_name=ghost, status=archived via real DB row
- 🟢 AC2: non-exec vs LIVE target refused loud naming both; state preserved — evidence: handlers_memory.go:363-367 emits permissionError(CodeForbidden, ...) with caller(%s) and target(%s); memory untouched asserted — test: TestDeleteMemory_NonExecCannotTargetLiveAgent internal/relay/delete_memory_admin_test.go:100 — PASSED; msg contains both peer+victim, victim-note NOT archived
- 🟢 AC3: dead-author path works for non-exec janitor across both dead-author flavours; AC was amended to mechanism-only per dispatcher note, test verifies exactly that — evidence: handlers_memory.go:391-397 targetAuthorIsDead treats GetAgent nil AND status inactive/deleted as dead; gate at :363 ORs callerIsExec with targetAuthorIsDead — test: TestDeleteMemory_JanitorArchivesDeadAndUnregisteredAuthor internal/relay/delete_memory_admin_test.go:125 — PASSED; covers BOTH inactive-registered (departed) and never-registered (anonymous) authors, archived_by=janitor, agent_name preserved
- 🟢 AC4: param is in schema and reaches the handler; no silent drop — evidence: internal/relay/tools.go:393 mcp.WithString("agent", ...) added to deleteMemoryTool; registered via toolset.go:171 — test: TestToolSchemaBudget internal/relay/toolsize_test.go:29 — PASSED; iterates all registry tools so a missing/malformed param would marshal-fail. 3 AC tests above also exercise the param reaching the handler end-to-end

## 5. Timeline

- round 1 → **reject** (review-9e1ab06b-cc3f-4862-bc8f-66623d0f15df)
- round 2 → **reject** (review-9e1ab06b-cc3f-4862-bc8f-66623d0f15df)
- round 3 → **approve** (review-9e1ab06b-cc3f-4862-bc8f-66623d0f15df)

**Approve-with-findings (follow-up):** go test -tags fts5: 818 passed, 12 pkgs, exit 0; 4 AC tests + 3 pre-existing tests green; round-1 case-fold ratchet intact at handlers_memory.go:348,361; agentColumns/scanAgent untouched. Per-AC: AC1/AC2/AC3/AC4 all green.

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `9e1ab06b-cc3f-4862-bc8f-66623d0f15df`._
