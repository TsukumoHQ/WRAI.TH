# relay bugs

## ✅ RESOLVED — branch `fix/uat-p0`

Every bug below is fixed. Map (bug → fix location):

| Bug | Fix |
|-----|-----|
| spawn_children.prompt never pruned (86% of DB) | `db/spawn.go` UpdateSpawnChild clears prompt on completion + `PurgeSpawnChildren(7d)` wired in `relay/cleanup.go` |
| no concurrency cap on spawn fan-out | `spawn/manager.go` semaphore `MAX_CONCURRENT_SPAWNS` (default 10) |
| token_usage + cycle_history never pruned | `relay/cleanup.go` wires `PurgeOldTokenUsage`/`PurgeCycleHistory` + `idx_token_usage_created` |
| session_context: unbounded lists | `LIMIT` added to ListAgents/Boards/AllBoards/CustomEvents/ActiveElevations/FileLocks/Schedules×2/Triggers/Orgs/Skills/Cycles/Workflows |
| session_context: GetGoalCascade N+1 | `db/goals.go` single `goalProgressByProject` GROUP BY query |
| session_context: GetThread unbounded recursion | `db/messages.go` depth<200 CTE + LIMIT 200 + bounded root walk |
| HandleListAgents no cap/truncation | `db/agents.go` LIMIT 500 + `relay/handlers.go` description→200 chars |
| dispatched_by_me cancelled (300k) + leaks 2-5 | `db/tasks.go` status filter+LIMIT, `relay/project.go` desc trunc, ListConversations LIMIT 30, goalContextCap |
| vault docs bloat spawn context | `spawn/prompt.go` head+tail cap `RELAY_VAULT_DOC_MAX_BYTES` (20k) |
| pool-spawn nukes reports_to / re-register regress | `db/agents.go` preserves reports_to on respawn when nil |
| spawn prompt missing task_id | `spawn/prompt.go` surfaces `**task_id:**` |
| task.dispatched stranded on pool-full | `relay/handlers.go` re-fires oldest pending on task.completed |
| register_profile wipes unspecified fields | PUT `/api/profiles/:slug` merges from existing |
| PUT triggers/schedules no-op | `api_spawn.go` apiUpdateTrigger / apiUpdateSchedule PATCH |
| child stdout/stderr not persisted | `db/spawn.go` stdout_tail/stderr_tail (2KB/4KB) |
| trigger_cycle generic error | `api_spawn.go` propagates `cause` |
| update_task progress invisible | `db/task_progress.go` + `api.go` surfaces notes on task detail |
| **session_context unread bodies untruncated (81k payload)** | `relay/project.go` `projectMessages`: 300-char preview, P0-bypass budget (6k), `unread_omitted` trailer |

---

## BUG — spawn_children.prompt stored full + never pruned (86% of relay.db size)

**Severity:** P0 (disk + backup cost; observed 42MB DB where 36MB = this one table)
**Found:** 2026-04-21 00:30Z
**Impact:** Every spawn stores the **complete assembled prompt** (vault injections + context_pack + task description + constraints + cycle + identity) in `spawn_children.prompt`. Average 88KB per row, max observed 290KB. 392 rows = 36MB. Growth is linear with spawn count: 100+ MB per week on a busy project, multi-GB per year.

No `PurgeSpawnChildren` function exists. `cleanup.go` doesn't touch this table. VACUUM doesn't help (rows are live).

**Fix (ranked):**

1. **Clear prompt on completion.** `UPDATE spawn_children SET prompt='' WHERE id=?` on status transition to `finished`/`killed`/`failed`. Preserves row for audit, frees the payload. The full prompt lives in `/private/tmp/relay.log` already for same-day debug.
2. **Store hash + reassemble on demand.** `prompt_hash` column (SHA256). `BuildSpawnContext(profile, cycle, task_id)` is pure → same inputs always produce the same prompt. If debug needed, reconstruct.
3. **Add `PurgeSpawnChildren(maxAge)` wired in cleanup.go** — DELETE rows for finished/killed/failed spawns older than 7 days. Running children preserved.
4. **Don't store prompt at all.** Reconstructible from (profile_slug, cycle_name, task_id). Store just the triple.

Recommended: do #1 (easy, immediate) + #3 (age out completed rows after N days).

**Workaround I applied (irreversible — 370 old prompts cleared):**
```sql
UPDATE spawn_children SET prompt='' WHERE id NOT IN (SELECT id FROM spawn_children ORDER BY started_at DESC LIMIT 20);
VACUUM;
```
Result: 42MB → 9.2MB instantly. New spawns continue to store prompts normally. Old debug data lost — next iteration of the bug will happen in a week if fix not merged.

---

## BUG — no concurrency cap on spawn fan-out

**Severity:** P2 (operational, not security)
**Found:** 2026-04-21
**Impact:** `dispatcher.fireTriggers` + `SpawnManager.SpawnWithContext` enforce per-profile pool size, but NO global limit on concurrent claude subprocesses. If 15 triggers fire simultaneously (batch dispatch), 15 claude CLI processes spawn at once. Each is multi-GB RAM + parallel MCP calls + API rate hits.

**Fix:** add a `MAX_CONCURRENT_SPAWNS` env (default 10) + semaphore-gated spawn in `manager.go`. Excess waits or returns with "relay saturated, retry".

---

## BUG — DB tables grow forever (token_usage + cycle_history never pruned)

**Severity:** P1 (slow leak; becomes P0 after weeks of continuous use)
**Found:** 2026-04-21
**Impact:** `PurgeOldTokenUsage()` function EXISTS in `internal/db/token_usage.go` but is **never called** anywhere in the codebase (`grep` confirms 0 call sites). `cycle_history` has no prune function at all. After 2 days of overnight work: 3742 token_usage rows + 1046 cycle_history rows. Linear growth. Every `CheckQuota(tokens)` scans the full table (no time-bucket index).

**Fix:**
1. Wire `PurgeOldTokenUsage(30*24*time.Hour)` into a relay cleanup cron (hourly).
2. Add `PurgeCycleHistory(maxAge)` function + wire similarly.
3. Optional: add index `CREATE INDEX idx_token_usage_created ON token_usage(created_at)` — quota `CheckQuota` will scan fewer rows.

Workaround today: manual SQL `DELETE FROM token_usage WHERE created_at < datetime('now', '-30 days')` in a cron job.

---

## BUG — session_context audit (full): 10 memory leaks + N+1 queries

**Scan date:** 2026-04-21 00:20Z — scanned every `internal/db/*.go` list function.

### UNBOUNDED LISTS (no LIMIT clause, caller has no way to opt-out)
`ListAgents`, `ListBoards`, `ListAllBoards`, `ListCustomEvents`, `ListActiveElevations`, `ListFileLocks`, `ListSchedulesByProject`, `ListSchedulesByAgent`, `ListTriggers`, `ListOrgs`, `ListSkills`, `ListConversations`, `GetAgentTasks` (3 queries), `ListCycles`, `ListQuotas`, `ListWorkflows`, `ListWorkflowRuns`.

**Add `LIMIT N ORDER BY ...` to each.** Default 50-200 depending on cardinality. Most of these are admin/UI endpoints, low impact unless the project is huge; but the session_context path hits `ListConversations` + `GetAgentTasks` on every agent spawn.

### N+1 QUERY PROBLEM
`GetGoalCascade` — loops over all goals and runs `GetGoalProgress(g.ID)` per goal. **Fix:** single aggregate SQL with `GROUP BY goal_id` + LEFT JOIN tasks.

### RECURSIVE CTE WITHOUT DEPTH LIMIT
`GetThread` — recursive CTE follows `reply_to` unbounded. A malicious or buggy reply chain = OOM. **Fix:** `WITH RECURSIVE thread AS ( ... ) ... LIMIT 200` or add a depth counter `WHERE depth < 50`.

### SERVER HANDLER RETURNS RAW DB LISTS WITHOUT SIZE CAP
`HandleListAgents` returns full list. **Fix:** paginate + truncate agent.description to 200 chars.

---

## BUG — session_context audit: 4 memory leaks found

**Scan date:** 2026-04-21 00:20Z
**File:** `internal/relay/handlers.go` + `internal/db/tasks.go` + `internal/db/conversations.go`

Session_context is returned by EVERY `register_agent` and `get_session_context` call. Caps matter.

### Leak #1 — dispatched_by_me includes cancelled tasks (300k payload) — P0 [see below]
### Leak #2 — GetAgentTasks has NO LIMIT clause
`internal/db/tasks.go:236,245,256` — 3 queries, zero `LIMIT`. If CTO has 500 open dispatches, all 500 returned. **Fix:** add `LIMIT 50` to each + `ORDER BY priority DESC, dispatched_at DESC`.

### Leak #3 — full task descriptions embedded
Each task row in session_context includes the full `description` column. Some of our CTO briefs are 5-15k chars. 10 tasks × 10k = 100k wasted per session_context fetch. **Fix:** in the session_context SELECT, truncate description to 200 chars + add ellipsis marker `"desc_truncated":true`. Agent can call `get_task(id)` for full text on demand.

### Leak #4 — ListConversations unbounded
`internal/db/conversations.go:ListConversations` — no `LIMIT` clause, returns every conversation an agent is in. **Fix:** `LIMIT 30 ORDER BY created_at DESC`.

### Leak #5 — goal_context chains ancestry recursively
`handlers.go:2608-2621` — for each unique goal_id, calls `GetGoalAncestry` which recurses up the goal tree. OK in principle but unbounded by number of unique goals per agent. **Fix:** cap `len(goalContext) <= 10`.

## BUG — session_context.pending_tasks.dispatched_by_me returns CANCELLED tasks (300k payload)

**Severity:** P0 (breaks spawns, burns tokens, exceeds MCP output limit)
**Found:** 2026-04-21 00:13Z
**Impact:** `register_agent` returns a `session_context.pending_tasks.dispatched_by_me` list that includes every task the agent has EVER dispatched, regardless of current status — 66 cancelled + 1 in-progress + 2 blocked = 69 tasks in the CTO's payload. Total response: **303k characters** (exceeds MCP output limit 299,512 bytes). The list name says "pending_tasks" but it's actually "all_tasks_dispatched_by_me including cancelled/done/failed".

**Observed:** MCP client aborts with "result exceeds maximum allowed tokens" error — the agent cannot even complete its register_agent boot step. Every spawn wastes the preamble tokens and fails. This explains many of the "exit=1 in 3-9s" children we've seen.

**Fix:**
1. `session_context.pending_tasks.dispatched_by_me` WHERE status IN ('pending', 'in-progress', 'blocked') — filter out cancelled/done/failed at query time.
2. Cap the list at e.g. 20 items with a "+N more" trailer.
3. Summarize each task to title + id (drop description) — description can be fetched via `get_task(id)` on demand.

Priority: ANY of the 3 above would unblock. Even just #3 reduces a 14k-char task entry to ~80 chars.

File: likely `internal/db/tasks.go` or `internal/relay/session_context.go` — look for where `dispatched_by_me` is populated.

Workaround today: cancel-in-bulk obsolete dispatches. Our CTO accumulated 66 cancelled dispatches from the pre-restructure team = dead weight in every session_context.

---



## DESIGN GAP — relay lets vault docs bloat spawn context unchecked

**Severity:** P1 (design, not code bug — biggest real-world burn driver)
**Found:** 2026-04-20
**Impact:** the relay auto-injects every file in a profile's `vault_paths` at full content into each spawn. No cap, no warning. A profile with `["PROGRESS.md", "self-review-*.md", "USER_STORIES.md"]` can pull 250-400KB per spawn. 100 spawns/day × 300KB = 30MB of redundant doc reload, on top of the 67KB MCP tools list.

The relay is supposed to be the OS for agents — it should prevent doc dérive like a kernel limits memory per process. Currently it's a blind file-concatenator.

**Fix options (any one would help):**

1. **Tail cap per doc.** Config `RELAY_VAULT_DOC_MAX_BYTES=20000`. On injection, if `len(doc) > max`, keep header (first 200 bytes) + tail (last `max-200` bytes) + marker `<!-- N KB truncated, use get_vault_doc for full -->`. Deterministic, invisible to agents that don't need history.

2. **Index mode flag on `vault_paths`.** Allow `["PROGRESS.md:index"]` → inject only `path + size + last-modified + first-heading` so agent knows the doc exists and can `get_vault_doc` if needed. Pay-for-what-you-use.

3. **Validation at `register_profile` time.** Sum the current size of every declared path. If total > threshold (say 50KB), reject the registration and list the biggest offenders. Forces profile authors to think.

4. **Per-spawn budget telemetry.** Log `spawn X: vault_context=150KB (PROGRESS.md=75KB, self-review-cto=60KB, ...)`. Make the waste visible. Without measurement, we optimize blind.

Fix 1 is ~10 LoC in `internal/spawn/prompt.go` around the vault-inject loop. Would single-handedly make the "my docs got huge" problem silent instead of expensive.

---

## BUG — pool-spawn burst nukes reports_to to NULL on ALL spawned agents

**Severity:** P0 (breaks org hierarchy + authorization)
**Found:** 2026-04-20 — observed 10+ times
**Reproduced:** every mass pool-refill via webhook POST /api/webhooks/<project>/task.dispatched

**Impact:** when 10 profiles get `task.dispatched` fired in <1s (manual pool-refill workaround), the relay spawns 10 children. Each spawn calls internal `register_agent(name, ...)` to upsert the child's registration. The internal call does NOT pass `reports_to` → relay code defaults it to NULL → ALL 10 pre-existing reports_to fields get wiped. A cron healer at */5min cannot keep up because the next pool-refill re-wipes them.

**Root cause:** `internal/spawn/assembly.go` calls `db.RegisterAgent(name, role, ...)` without forwarding an existing `reports_to`. The `resolveReportsTo()` fallback chain only picks "sole-exec" when creating NEW agents, not when updating existing. So every re-register blanks the field.

**Fix (ranked):**
1. In the spawn register-agent call, read the existing row's `reports_to` first and pass it through. Never pass NULL for an existing agent unless explicitly intended.
2. Add `reports_to` column to profiles; spawn uses `profile.reports_to` for new agents.
3. Separate `upsert_agent(reports_to)` from `touch_agent(last_seen)`: spawn should use touch, not upsert.

---

## BUG — spawn prompt does not include `task_id` of the dispatched task

**Severity:** P0 (breaks the task state machine)
**Found:** 2026-04-20
**Impact:** Spawned children cannot call `claim_task(task_id=...)` because they don't know their task_id. The `## Task` section in `internal/spawn/prompt.go:377` shows `[priority] title + description + acceptance` but never surfaces the UUID. Across 30+ spawns, **zero tasks got `claimed_by` populated** — state machine "in-progress" is set automatically by the spawn trigger, but no agent explicitly claimed.

**Fix:** in `internal/spawn/prompt.go` around line 378 (the `## Task` section), add:
```go
fmt.Fprintf(&b, "**task_id:** `%s`\n\n", ctx.Task.ID)
```

Workaround today: agents call `list_tasks(assigned_to=<self>, status=in-progress)` → pick matching title → use that ID.

---

## BUG — task.dispatched trigger fires once; pool-full → task stranded

**Severity:** P1 (pending tasks sit forever)
**Found:** 2026-04-20
**Impact:** when `dispatch_task` is called and the target pool is already full, the `task.dispatched` trigger STILL fires but the spawn call returns "pool full". The task stays `pending` forever. When the pool later frees (child completes), NO retry happens — the trigger does not re-fire.

**Fix:** on `task.completed` for profile X, check if any `pending` tasks exist for profile X (or same pool). If yes, fire task.dispatched for the oldest one.

Workaround: cron pool-refill every 3min that fires task.dispatched webhook for one pending task per profile.

---

## BUG — `register_profile` wipes unspecified fields

**Severity:** P1 (data loss on partial update)
**Found:** 2026-04-20
**Impact:** Calling `register_profile` with only a subset of args (e.g. just `name` + `vault_paths`) nukes `context_pack`, `exit_prompt`, `role`, `allowed_tools`, `pool_size` back to empty.

**Fix:** add a separate `update_profile` MCP tool with PATCH semantics, or have `register_profile` merge from existing row when the profile already exists.

Workaround: always pass ALL fields on re-register (fetch existing first, merge, re-post).

---

## BUG — `reports_to` regresses on agent re-register when field not passed

**Severity:** P1
**Found:** 2026-04-20
**Impact:** `register_agent(name=X)` without passing `reports_to` resets the existing `reports_to` to "sole-exec" fallback (ceo) instead of preserving the existing value. Same family as the register_profile bug.

**Fix:** same as the spawn bug above (preserve existing on re-register unless explicitly overriding).

---

## BUG — `PUT /api/triggers/{id}` silently no-ops

**Severity:** P2
**Found:** 2026-04-20
**Impact:** Sending `PUT /api/triggers/{id}` with `{"max_duration":"20m"}` returned success but the trigger stayed unchanged. The relay has no PUT/PATCH handler for triggers, so the request falls through to a 200 that doesn't persist.

**Fix:** add `apiUpdateTrigger` handler in `api_spawn.go` mirroring `apiUpdateSchedule`, OR return 405 for unsupported methods.

Workaround: DELETE + recreate the trigger.

---

## BUG — `PUT /api/schedules/{id}` with partial body silently no-ops

**Severity:** P2
**Found:** 2026-04-20
**Impact:** `PUT /api/schedules/<id>` with `{"enabled":false}` returns success but the schedule stays `enabled=true`. Same family as the triggers PUT bug.

**Fix:** make PUT a real PATCH that merges, or reject partial bodies with 400.

Workaround: DELETE + recreate.

---

## BUG — child stdout / stderr not persisted

**Severity:** P2 (debuggability)
**Found:** 2026-04-20
**Impact:** when a child exits=1 after 3-9 seconds, the relay logs `error="exit status 1"` but nothing else. The `spawn_children` table has columns for `prompt`, `exit_code`, `error` (one line) — but no `stdout` / `stderr`. Impossible to diagnose "why did this batch of 6 children die in 4s at cron-boundary :00:00".

**Fix:** add `stdout_tail` (last 2KB) + `stderr_tail` (last 4KB) columns, truncated on insert.

---

## BUG — `trigger_cycle` returns generic "trigger failed" on any error

**Severity:** P2
**Found:** 2026-04-20
**Impact:** `POST /api/schedules/<id>/trigger` returns `{"error":"trigger failed"}` without distinguishing: schedule not found, profile missing, lock held, quota exceeded, spawn internal error. Debugging requires grepping `/private/tmp/relay.log`.

**Fix:** propagate the underlying error: `{"error":"trigger failed","cause":"profile 'ceo-autopilot' not found"}`.

---

## BUG — no `update_task` progress visibility

**Severity:** P2 (observability)
**Found:** 2026-04-20
Children running long (5-20min) can't signal progress to the board between claim and complete. `update_task(task_id, progress_note)` exists but doesn't surface anywhere visible in the web UI.

**Fix:** surface progress notes in the activity feed + task detail view.
