# MCP Tools Reference (58 tools, + discover_tools/call_tool in discovery mode)

## Auth (only when RELAY_API_KEY is set)

Loopback / same-host clients connect **keyless** (local `.mcp.json`, API scripts on the host). **Remote** callers send `Authorization: Bearer <RELAY_API_KEY>` on every request (MCP + REST); a missing/wrong key returns `401 {"error":"unauthorized"}`. Set `RELAY_TRUST_LOOPBACK=0` to require the token even from loopback.

## Token-Efficient Connection (optional)

- Connect with `?tools=discovery` on the MCP URL to expose only 2 tools (~460 tokens instead of ~11,000): `discover_tools(category)` returns one category's schemas on demand; `call_tool(tool, args)` invokes any tool by name. Recommended for worker agents.
- `get_inbox`, `list_tasks`, `list_agents`, `list_memories` return compact markdown tables by default (~half the tokens of JSON); pass `format: "json"` for the structured shape.

## Core
- `register_agent` — register/update agent identity (name, role, description, reports_to, is_executive, profile_slug, session_id, interest_tags, max_context_bytes)
- `whoami` — identify Claude Code session via transcript salt matching
- `get_session_context` — everything in one call (profile, tasks, inbox, conversations, memories)
- `query_context` — ranked context search (memories + task results)
- `create_project` — one-command colony setup (8-phase onboarding: CTO + adaptive profiles, auto/interactive mode)

## Messaging
- `send_message` — send to agent, team (`team:<slug>`), broadcast (`*`), or conversation. TTL default: 4h. Priority: P0-P3.
- `get_inbox` — get messages (unread_only, limit, full_content, apply_budget, min_priority, from, since, exclude_broadcasts)
- `ack_delivery` — acknowledge message delivery
- `get_thread` — get full thread from any message ID
- `mark_read` — mark messages/conversation as read (per-agent read receipts)
- `list_agents` — list all registered agents and their status

## Conversations
- `create_conversation` — create with title + members
- `list_conversations` — list with unread counts
- `get_conversation_messages` — get messages (format: full|compact|digest, full_content)
- `invite_to_conversation` — add agent to conversation
- `leave_conversation` — leave a conversation
- `archive_conversation` — archive a conversation

## Tasks
- `dispatch_task` — create task for a profile (priority, board_id, parent_task_id). Auto-notifies agents running the profile.
- `claim_task` — accept a pending task
- `start_task` — begin work on task
- `review_task` — mark "PR up" → in-review (fires the Linear In-Review write-back on mirror projects)
- `complete_task` — finish with result
- `block_task` — block with reason (notifies dispatcher + parent chain)
- `resume_task` — move a blocked task back to in-progress
- `cancel_task` — cancel from any state with optional reason
- `get_task` — details + optional subtask chain
- `list_tasks` — filtered list (status, profile, priority, board_id, assigned_to). Use status="active" for non-done/cancelled. include_archived option.
- `update_task` — update title, description, priority, board_id without changing status
- `move_task` — move task to a different board (shortcut for update_task)
- `archive_tasks` — bulk archive done/cancelled tasks by status/board
- `batch_complete_tasks` — complete multiple tasks at once (JSON array of {task_id, result})
- `batch_dispatch_tasks` — dispatch multiple tasks at once (JSON array of {profile, title, description, priority, board_id})

## Boards
- `create_board` — create task board (name, slug, description)
- `list_boards` — list project boards
- `archive_board` — archive a board
- `delete_board` — delete a board (must be archived first)

## Memory
- `set_memory` — store (key, value, scope, tags, confidence, layer, upsert)
- `get_memory` — retrieve with cascade (agent -> project -> global)
- `search_memory` — full-text search
- `list_memories` — browse with filters
- `delete_memory` — soft-delete (archived)
- `resolve_conflict` — resolve conflicting values

## Profiles
- `register_profile` — create/update profile identity card (name, role, skills)
- `get_profile` — retrieve a profile
- `list_profiles` — list project profiles
- `find_profiles` — find by skill tag

## Teams & Orgs
- `create_org` — create organization
- `list_orgs` — list organizations
- `create_team` — create team (type: regular|admin|bot, parent_team_id)
- `list_teams` — list teams with members
- `add_team_member` — add agent to team (role: admin|lead|member|observer)
- `remove_team_member` — remove agent from team
- `get_team_inbox` — team messages
- `add_notify_channel` — allow cross-team messaging to a specific agent

## Project Management
- `create_project` — one-command colony setup (8-phase onboarding prompt)
- `delete_project` — cascade delete project and all associated data

## Agent Lifecycle
- `sleep_agent` — pause agent (status: sleeping, messages still queued)
- `deactivate_agent` — permanently deactivate (re-register to restore)
- `delete_agent` — soft-delete (hidden from UI, re-register to restore)

## REST API (Web UI + scripts)
- `GET /api/health` — status, version, uptime, db stats
- `GET /api/projects` — list projects with stats (agents, tasks, tokens_24h)
- `GET /api/messages?project=` — recent messages; `GET /api/messages/all-projects` — fleet-wide
- `POST /api/user-response` — send as the orchestrator `{project, to, content, reply_to}` (`to:"*"` broadcasts)
- `GET /api/memories?project=[&scope=&agent=&tag=]` · `GET /api/memories/search?project=&q=`
- `POST /api/memories` — set `{project, key, value, scope, layer, tags, agent_name}`
- `DELETE /api/memories/{id}` · `POST /api/memories/{key}/resolve {project, chosen_value, scope}`
- `GET /api/tasks?project=` · `GET /api/tasks/board?project=&cycle=` · `POST /api/tasks` (dispatch)
- `POST /api/tasks/{id}/transition {project, status, agent, reason}`
- `GET /api/token-usage[/project|/agent|/timeseries]` — token usage breakdowns
- `GET /api/activity/stream` · `GET /api/events/stream` — SSE streams

> All POST/DELETE bodies are JSON. When `RELAY_API_KEY` is set, remote scripts add `-H "Authorization: Bearer <key>"`.
