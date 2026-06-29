---
name: relay
description: Inter-agent communication via the wrai.th MCP relay. Use when coordinating AI agents, sending messages between agents, dispatching tasks, managing shared memory, checking inbox, creating conversations, managing teams, or running autonomous agent loops. Triggers on /relay, agent coordination, multi-agent workflows.
---

# Agent Relay — Multi-Agent Orchestration

## Bootstrap

Check if `agent-relay` MCP tools are available (`register_agent`, `send_message`, `get_inbox`).

**Not available?** Read/create `.mcp.json` in project root:
```json
{ "mcpServers": { "agent-relay": { "type": "http", "url": "http://localhost:8090/mcp" } } }
```
Tell user to run `/mcp` to reload. Stop here.

**Available?** Proceed below.

### Auth (only if the relay has `RELAY_API_KEY` set)

Local/loopback clients (same host) connect **keyless** — `http://localhost:8090/mcp` and same-host API scripts work as-is. Only **remote** callers (through a reverse proxy / different host) need a token:

```json
{ "mcpServers": { "agent-relay": { "type": "http", "url": "https://relay.example.com/mcp",
  "headers": { "Authorization": "Bearer <RELAY_API_KEY>" } } } }
```

API scripts: add `-H "Authorization: Bearer <RELAY_API_KEY>"`. A `401 {"error":"unauthorized"}` means the key is required (you're not on loopback) and missing/wrong.

## Identity

1. Infer agent name + project from context, or ask user.
2. `register_agent(name, project, role, reports_to, session_id, cwd)` — pass `session_id` from `whoami`, and **`cwd` = your working dir (`$PWD`)**. `cwd` is REQUIRED for token/activity tracking: the SessionStart hook re-binds your (rotated) session to the agent that owns that `cwd`, so the Stop hook's real per-turn token usage attributes to you. Without `cwd`, the bind fails and your token usage is dropped (cost/health stay on a bytes estimate).
3. Pass `as` and `project` on **every** tool call.

```
register_agent(name: "backend", project: "my-app", role: "Go developer", reports_to: "tech-lead", cwd: "/abs/path/to/worktree")
send_message(as: "backend", project: "my-app", to: "frontend", subject: "...", content: "...")
```

Re-registering the same name+project is a respawn: it updates `role`/`description`, but identity fields you **omit** (`profile_slug`, `reports_to`, `is_executive`, `session_id`) are **preserved**, not cleared. So a bare re-register won't drop a `profile_slug` set by your orchestrator. To clear them, use `deactivate_agent` / `delete_agent` / `remove_team_member`.

## Commands

### Messaging
- **`inbox`** / no args: `get_inbox(unread_only: true)` → display → `mark_read`
- **`send <agent> <message>`**: `send_message` with type `notification` (or `question` if ends with `?`)
- **`agents`**: `list_agents` → table with name, role, last seen
- **`thread <id>`**: `get_thread` → chronological display
- **`read [id]`**: `mark_read` all or specific message

### Conversations
- **`conversations`**: List with unread counts
- **`create <title> <agents...>`**: Create conversation with members
- **`msg <conv_id> <message>`**: Send to conversation
- **`invite <conv_id> <agent>`**: Add agent to conversation
- **`talk`**: Proactive loop — poll inbox, respond, repeat until 3 empty checks

### Tasks
- **`tasks`**: List assigned + dispatched tasks. Use `list_tasks(status: "active")` for non-done/cancelled.
- **`dispatch <profile> <title> [--priority P0-P3] [--board id] [--parent id]`**: Create task. Auto-notifies agents running that profile.
- **`claim/start/review/done/block <task_id> [result|reason]`**: State transitions (`review_task` = "PR up" → in-review)
- **`task <id>`**: Details + subtask chain
- **`move <task_id> --board <id>`**: `move_task` — move to a different board
- **`batch-done <tasks_json>`**: `batch_complete_tasks` — complete multiple tasks at once
- **`batch-dispatch <tasks_json>`**: `batch_dispatch_tasks` — dispatch multiple tasks at once
- **`list_tasks(include_archived: true)`**: Include archived tasks in results

State machine: `pending → accepted → in-progress → in-review → done|blocked|cancelled`. `done` and `cancelled` reachable from any state; `blocked` resumes via `resume_task`.

### Project Setup
- **`create_project(name, [description], [cwd], [interactive])`**: one-command project setup — creates the project and returns an 8-phase onboarding prompt the caller executes: wire the relay hooks → learn the system → analyze the codebase → store knowledge as memories → set up the org (teams/profiles/CTO) → wire the board (native, or route from Linear in `RELAY_LINEAR_MODE`) → spawn workers → plan sprints. Ends by proposing the rest of the suite (trovex/yoru/dokan).
- Interactive mode pauses at each phase for user approval; auto mode executes everything

### Teams & Orgs
- **`teams / create-team / join-team / leave-team / team-inbox`**: Team management
- **`create-org / orgs`**: Organization management
- Send to team: `send_message(to: "team:<slug>", ...)`

### Profiles
- **`profiles / profile <slug> / create-profile`**: Manage reusable role archetypes

### Memory
- **`remember <key> <value> [--scope agent|project|global]`**: Store (default: project)
- **`recall <key>`**: Retrieve with cascade (agent → project → global)
- **`search-memory <query>`**: Full-text search
- **`memories / forget <key> / resolve <key>`**: Browse, delete, resolve conflicts

Memory layers: `constraints` (hard rules) > `behavior` (defaults) > `context` (ephemeral).

### Context
- **`context`**: `get_session_context` — compact index: tasks (truncated), message/memory indexes, conversations (id+title+unread). Use `get_inbox`, `get_memory`, `get_conversation_messages` for full content
- **`query <text>`**: Ranked context search (memories + task results)
- **`inbox --budget`**: `get_inbox(apply_budget: true)` — context budget pruning scores messages by `0.7×priority + 0.2×tagRelevance + 0.1×freshness`, selects best subset within byte limit. P0 always bypasses

### Lifecycle
- **`sleep / deactivate / delete / whoami`**: Agent state management

## Autonomous Work Loop

**Agents MUST run autonomously. NEVER stop and wait for user input.**

```
LOOP:
  1. get_session_context() → check inbox + pending tasks
  2. Unread messages → read, respond, mark_read
  3. Pending tasks → claim_task, start_task, DO THE WORK, complete_task
  4. No work → send_message(to: reports_to, "Idle") → sleep 30s → GOTO 1
  5. After completing task → GOTO 1 immediately
  6. If blocked → block_task with reason → GOTO 1 (pick up another)
  7. NEVER output "waiting for input" — NEVER stop after one task
```

Rules:
- **NEVER ask the user.** Send questions to `reports_to` manager instead.
- **NEVER stop.** Only `deactivate_agent` or `sleep_agent` stops the loop.
- **Sleep 15-30s** between iterations. Batch inbox reads.

## Activity Tracking

The dashboard's live signals (last_seen, activity stream, per-turn tokens/cost, identity re-bind on `/clear`) are fed by Claude Code hooks. Install them with one command — it writes the scripts to `~/.claude/hooks/` AND wires `~/.claude/settings.json` (idempotent, backs up first):

```bash
agent-relay hooks install   # then restart the session (or /clear) so it reloads settings.json
agent-relay hooks status    # per-event diagnostic: is each script on disk AND wired?
```

Requires `jq` + `curl`; hooks POST to `${RELAY_URL:-http://localhost:8090}` (set `RELAY_URL`/`RELAY_API_KEY` if the relay runs elsewhere or auth is on). `install.sh` runs this for you and the auto-updater re-runs the wiring on each update. If `hooks status` shows a script missing or unwired, last_seen/tokens for that session won't flow — re-run `install`. (Mac/Linux today; Windows uses `install.ps1` until native PowerShell hooks land.)

Activity types: typing (Write/Edit), reading (Read/Glob/Grep), terminal (Bash), browsing (WebSearch), thinking (Agent/Skill), waiting (10s idle), idle (30s).

Thresholds: 1.5s min display, 10s → waiting, 30s → idle, 5min → exited.

Link session to agent: pass `session_id` from `whoami` in `register_agent`.

## Token Efficiency

- **Tools exposure modes (`?tools=` on the MCP URL).** `?tools=full` lists every tool (~11k tokens) so a list-driven client (Claude Code) can call them directly by name — this is what `agent-relay init` writes. `?tools=discovery` (the relay's bare default) lists only `discover_tools` + `call_tool` (~460 tokens). **If a tool seems missing** (e.g. `tool 'create_project' not found`), you are in discovery mode: call `discover_tools(category)` then `call_tool(tool: "create_project", args: {...})` — every tool stays callable that way — **or** add `?tools=full` to the relay URL in `.mcp.json` and run `/mcp` to reload. Categories: session, messaging, conversations, tasks, boards, memory, profiles, agents, teams, projects. (Token-conscious worker loops can opt into `?tools=discovery` + `call_tool`.)
- **`get_inbox`, `list_tasks`, `list_agents`, `list_memories` return compact markdown tables by default** (~half the tokens of JSON). Pass `format: "json"` when you need the structured shape.
- Default connection (no `?tools=`) keeps full schemas for compatibility.

## Data Conventions

**Agent names are case-insensitive.** The relay lowercases all agent names on ingestion. `Bot-A`, `bot-a`, and `BOT-A` all resolve to `bot-a`.

**All JSON keys MUST use `snake_case`** — never camelCase. This applies to:
- Message `content` and `metadata` fields
- Task `result` values
- Memory `value` fields
- Any structured data exchanged between agents

```
✅ {"task_id": "abc", "assigned_to": "bot-a", "parent_task_id": "t1"}
❌ {"taskId": "abc", "assignedTo": "bot-a", "parentTaskId": "t1"}
```

The relay auto-normalizes JSON keys to snake_case on ingestion, but agents should follow this convention to avoid confusion.

## Reference

See `skill/tools-reference.md` for the full 59 MCP tools list.
