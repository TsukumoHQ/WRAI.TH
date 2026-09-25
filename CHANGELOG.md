# Changelog

All notable changes to wrai.th are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/). Versions follow [Semantic Versioning](https://semver.org/).

## [1.22.0] — 2026-09-25

Obligations become first-class relay data: the ACK checker now runs on a norms/obligations engine with an escalation chain agents can see, discharge or decline, questions sent to agents become tracked obligations that escalate when unanswered, while messages keep their task links and reply chains after retention, and memory writes stop losing concurrent updates.

### Added
- Obligation tools for agents: `obligations_mine` lists what you owe (your profile pool plus tasks assigned to you, with deadline and escalation depth), `obligation_discharge` marks one fulfilled after the relay re-checks it, and `obligation_decline` hands it up the chain immediately with a reason class (`not_mine`, `cannot`, `blocked_by`, `duplicate`) (79f48b9e, d1361bf)
- `send_message` takes an optional `task_id`, stored as the message's task link; an unknown id or one that contradicts `metadata.task_id` is refused before anything is written, and so is a malformed `reply_to`. A well-formed `reply_to` whose parent is missing is still accepted, and the result now reports `reply_to_resolved` (fde40c25, b8f3d88)
- Messages are linked to their task at insert: `messages.task_id` is filled from `metadata.task_id` or inherited from the `reply_to` parent in the same project, and older rows are backfilled on start (da1945d5, c2051b3)
- Message tombstones: the retention purge leaves one content-free row per deleted message (ids, routing, priority, no subject or body) in the same transaction, so a purged id can still be told apart from a mistyped one (f53b2160, c6add48)
- New `policy` message type for standing doctrine: it never expires, is never dead-lettered or purged, and is shown in its own `policies` block at every boot until the agent acks it (adefc1f4, 0571352)
- `set_memory` takes an optional `based_on` (the memory id you read, auto-filled from your last `get_memory`). If someone wrote a newer version in between, both stay live as siblings, the result names the conflict, and the other author is told (1a031d43, da972fc)
- Answer obligations: a direct or team message sent with `action_required` `ask` or `decide` now opens one obligation per recipient, fulfilled automatically when that recipient replies (the reply chain is followed through purged messages too). Recipients see them in `obligations_mine` and can discharge or decline them. Broadcast asks and conversation messages are excluded, and only messages sent after the upgrade are covered (044a4876, 038d089)
- Unanswered questions escalate: an answer obligation still open after `answer_reply_age` (1 h) is marked missed and passes to the recipient's manager (their `reports_to`, else an active executive), and after a further `answer_role_age` (2 h) to the human, who is only ever the last step. Each step sends one P1 message quoting the question, as a reply to it, so answering that message counts; a late answer from the original recipient closes the steps opened for it. The original message is never touched (a01d0b87, 5e48a6e)
- Boot selection journal: `get_session_context` logs one `[budget]` line per boot with the candidate, selected and omitted message ids, so the inbox budget can be replayed from logs. The boot payload itself is unchanged (409fb2e8, 6b3441c)

### Changed
- The ACK checker runs on a new norms/obligations engine instead of hard-coded checks. Behaviour is identical to v1.21, proven by an equivalence test against the old checker (f77efe18, 266a026)
- ACK escalation is now a chain: a task left unclaimed notifies its dispatcher at 15 min, escalates at 45 min, goes to the dispatcher's manager (else an executive, else the founder) at 90 min, and reaches the human operator only at 4 h. Notices are stored messages instead of push-only, so an offline dispatcher still gets them, and a notify can no longer arrive after an escalation. Tasks already escalated before the upgrade send nothing new (6b4369f0, d215a0c)
- Integrity scan: a slow scan log line now names the slowest phase and check, `orphan_agent_profile` only flags active agents, and a task on a missing board with no re-home target is recorded once instead of being logged on every sweep (27a77033, 52a9abf)

### Fixed
- A task promoted from backlog, unblocked or reset no longer fires the whole ACK chain at once: the ACK clock restarts each time a task becomes pending (new `tasks.pending_since`) instead of counting from its original dispatch (c933b2f1, c5288ca)
- The ACK clock also restarts when a task goes back to pending through a watchdog requeue, an expired lease, or the deactivation of the agent holding it (58ece5e2, cef0f87)
- The ACK sanction log line again ends with the task age in minutes, lost when the sanction code was shared with `obligation_decline` (904e024d, 81ba213)
- A memory writer working from an old version no longer silently archives a newer concurrent write; a superseded version now gets its `valid_until` closed, and re-saving the same value with a new layer is no longer dropped (df33d619, c4956f8)
- Replies to a purged message keep their parent's trace and action instead of waking the recipient as a new task, and `reply_to` checks treat a tombstoned parent as resolved (93b6f1cf, 5845c2f)
- Inbox budget scoring: an unknown priority no longer scores as P0, an unparseable or future `created_at` no longer counts as the freshest, and agents without tags can reach a full score. `apply_budget` is still off by default, so this was latent (1f190795, 8978751)
- Test suite: the token-usage flusher is stopped before a test closes its database, which removes a data race on captured logs. No change in production (d230b752, 4cbaa4c)

### Upgrade notes
All schema changes are additive and applied automatically on first start. Older binaries ignore the new tables and column, so rolling back is safe.
- New table `message_tombstones` with index `idx_message_tombstones_reply`.
- New tables `norms` and `obligations` with indexes `idx_obligations_active`, `idx_obligations_bearer`, `idx_obligations_subject`. Seven norms are seeded if absent: `ack.notify`, `ack.escalate`, `ack.manager`, `ack.human`, `answer.reply`, `answer.role`, `answer.human`.
- New column `tasks.pending_since`, backfilled to `dispatched_at` for existing tasks.
- `messages.task_id` already existed; it is now written on every insert, and rows whose `metadata.task_id` names a task in the same project are backfilled on each start (a no-op after the first).
- New settings `ack_manager_age` (default 90 min) and `ack_human_age` (default 4 h), each clamped to 1 min to 48 h. The existing `ack_notify_age` and `ack_escalate_age` are unchanged.
- New settings `answer_reply_age` (default 60 min, clamped to 1 min to 24 h), the deadline recorded on each answer obligation, and `answer_role_age` (default 2 h, clamped to 1 min to 48 h), the time the manager gets before the question reaches the human.
- Only questions sent after the upgrade get answer obligations, so the first sweep sends nothing to the human for older messages.
- The first integrity scan after the upgrade closes open `orphan_agent_profile` rows that belong to inactive or deleted agents.

Full diff: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.21.1...v1.22.0

## [0.7.0] — 2026-04-19

### Added — new subsystems
- **Spawn engine** — `spawn`, `kill_child`, `list_children` MCP tools. Trigger-based auto-spawn: when a task is dispatched / completed / blocked, a matching trigger launches a `claude` headless child with the profile's assembled context (profile + vault + memories + task). Children visible in `spawn_children` table, killable via REST.
- **Event trigger dispatcher** — `triggers` table + `POST /api/triggers`, `POST /api/webhooks/:project/:event`. Events fan out to matching triggers with match_rules JSON, cooldown, and trigger_history audit log. Dot-notation event names (`task.dispatched`, `task.completed`, `task.blocked`, `task.resumed`, `message.received`, `signal.interrupt`, `signal.alert`) with backward-compat aliases for the legacy underscore form.
- **Webhook receivers** — `POST /api/webhooks/:project/:event` to fire triggers from external systems.
- **Poll triggers** — `POST /api/poll-triggers` background worker polls external URLs at configurable interval, evaluates JSONPath conditions (`eq`, `neq`, `contains`, `gt`, `lt`), fires events when matched. `POST /api/poll-triggers/:id/test` returns detailed error on misconfig.
- **Signal handlers** — `POST /api/signal-handlers` creates a trigger with `event=signal:<name>` as a shortcut for reacting to signal events.
- **Skill registry** — `skills` + `profile_skills` tables. Replaces LIKE-based matching. `dispatch_task` accepts `required_skill` to auto-resolve the best profile. `find_profiles` supports `skill_name` (JOIN) and `skill_tag` (LIKE).
- **Per-agent quotas** — `agent_quotas` table. `max_tokens_per_day`, `max_messages_per_hour`, `max_tasks_per_hour`, `max_spawns_per_hour`. Enforced on send_message / dispatch_task / spawn. PUT /api/quotas/:agent echoes the full quota object.
- **Web terminal** — `POST /api/terminal/spawn` launches an interactive `claude` session with a profile's context_pack, WebSocket at `/api/terminal/ws/:id`.
- **Command panel** — REPL-style input in the web UI.
- **Cross-project exec DM** — `send_message(target_project:"colony-b")` delivers to an executive agent in another project. Both sender and recipient must have `is_executive=true`. Message lives in target_project scope; metadata records `source_project`, `source_agent`, `cross_project:true`.
- **`resume_task` MCP tool** — transitions a blocked task back to in-progress. Fires `task.resumed`.
- **Activity events** — `GET /api/events/recent?project=X&limit=N` returns the last 500 MCP events from an in-memory ring buffer. `send_message` now emits `message.broadcast` / `message.team` / `message.conversation` / `message.send` / `message.cross_project` events.

### Added — polish
- `add_notify_channel` MCP tool — allowlist cross-team DM at the agent level.
- `ack_delivery` accepts `message_id` as a fallback (resolves via `AcknowledgeDeliveryByMessage`).
- `mark_read` accepts singular `message_id` as a one-element array.
- `batch_complete_tasks` accepts `task_ids:[...]` shorthand in addition to `tasks:[{task_id:...}]`.
- `claim_files` response includes `existing_claims` and `conflict:true` when overlapping a lock.
- `POST /api/workflows` flags unknown fields via a `Warning` header.
- `create_project` auto-creates the `default` project row on first boot.
- FTS5 escape: `search_memory` and `search_vault` tolerate hyphens and other punctuation in queries (no more `no such column: machine`).

### Fixed
- **`agent-relay update` downgrade prevention** — refuses dev/unknown builds, compares semver before overwriting, warns + asks for `--force` when local is ahead of the latest release.
- **CLI `send` creates deliveries** — CLI-sent messages now appear in `inbox` (was silent no-op because `CreateDeliveries` was skipped). Also validates non-empty `from` and rejects self-send.
- **Budget pruning preserves dropped messages** — `get_inbox(apply_budget:true)` only marks survivors as `surfaced`. Messages dropped by the budget stay `queued` and reappear on the next poll (was: all fetched → all surfaced, rejected ones lost forever).
- **Spawn headless injects profile `vault_paths`** — the documented auto-injection promise now holds for `ModeHeadless` spawns (triggers, REST, MCP `spawn`). Both explicit vault_paths and FTS hits are merged with deduplication.
- **`list_children` / `/api/spawn/children` without `agent`** — skip the `parent_agent` filter when no parent specified; returns all children in the project.
- **Memory version race under concurrency** — `set_memory` wraps read-modify-write in `BEGIN IMMEDIATE` so concurrent writers on the same key get distinct sequential versions (was: duplicate versions, broken `supersedes` chain).
- **Trigger cooldown burst race** — new `ClaimTriggerFire` atomic `UPDATE ... WHERE last_fired_at < threshold` ensures exactly one winner per cooldown window. 10 parallel webhooks → 1 fire + 9 cooldown skips (was: 3 fires).
- **Version dynamically propagated** — `/api/health.version` and MCP `serverInfo.version` now reflect `main.Version` (was: hardcoded `"0.5.0"`).
- **Goal cascade rollup** — `get_goal_cascade` now aggregates `total_tasks` / `done_tasks` / `progress` across the entire descendant tree. A project_goal with 3 busy agent_goals reports 15/20 not 0/0.
- **`PUT /api/profiles/:slug` merges** instead of replacing. Absent fields keep their current value (was: `context_pack` wiped when UI sent only `{name, role}`).
- **`/api/tasks/latest`** default window raised from 30s to 1h.
- **`cooldown_seconds:0`** respected (was: silently defaulted to 60).
- **Migration deliveries** — pre-existing messages `<24h` old backfilled as `queued` not `surfaced`, so a dev running `send` then restart doesn't lose their messages.
- **CLI `stats` without `-p`** labels the scope (`project: default (default — use -p <name> to scope)`).
- **CLI `memories -s "hyphenated-term"`** no longer crashes on FTS5 parsing.
- **Schedule tool error** lists all missing fields (`name`, `cron_expr`, `cycle-or-prompt`) instead of one at a time.
- **Trigger cooldown drops** now recorded in `trigger_history` with `error: cooldown (Xs)` instead of silent skip.
- **Poll-trigger test** endpoint returns `details` field with the underlying error.

### Changed — breaking (with compat aliases)
- **Internal event names** switched to dot notation. Old underscore names (`task_pending`, `task_completed`, etc.) still match triggers registered under either form thanks to `eventAliases` map. New code should use dot notation.

### Performance
- Tested under concurrent load: 10 agent registrations / 140ms, 200 messages / 350ms (~570 msg/s), 500 memory inserts / 570ms, FTS5 search / ~237ms on 500 rows, 20 concurrent dispatch+complete / 280ms round-trip. No deadlocks, no orphan deliveries, no race-duplicated rows after fixes.

### Docs
- README MCP tool count corrected to 76 (was 67).
- `register_agent` description updated: broadcasts are enforced once any team exists in the project (bootstrap mode before that).
- Cross-project DM documented in `send_message` tool description.

## [0.5.0] — 2026-03-11

### Added
- **`/health` REST endpoint** — uptime, version, DB row counts for monitoring
- **`move_task` MCP tool** — move task to different board/goal with prefix resolution
- **`batch_complete_tasks` MCP tool** — complete multiple tasks in one call
- **`batch_dispatch_tasks` MCP tool** — dispatch multiple tasks in one call
- **`list_tasks` filters** — `status: "active"` excludes done/cancelled, `include_archived` boolean
- **Auto-notifications** — dispatching a task sends an inbox message to agents running the target profile
- **Inline checklist** — interactive checkboxes on kanban cards, toggle items without opening the edit form

### Changed
- Default message TTL raised from 1h to **4h** (14400s)

### Fixed
- `GetAgentTasks` and `GetUnackedTasks` now exclude archived tasks
- `apiError` uses `json.Marshal` for proper JSON escaping
- Board dropdown in edit form showed empty names (`b.title` → `b.name`)
- `assigned_to` field value was never saved in edit form

## [0.4.0] — 2026-03-10

### Added
- **Kanban board** — full task management UI with drag-and-drop, board/goal columns, edit form with checklist and larger textarea
- **Pixel holo UI assets** — 9-slice panels, buttons, dividers, loading wheel, icon sets
- **Cascade delete** — deleting a project removes all related agents, tasks, messages, memories, boards, goals, conversations

### Fixed
- Vault `indexFile` errors now logged instead of silently swallowed during full reindex
- Duplicate `ZOOM_STEPS` and scale-btn declarations removed
- CRLF line endings in installer scripts
- Installer wrapped in block for `curl | sh` pipe compatibility
- Nil pointer in `DB.Close()` when opened read-only
- Release workflow made idempotent with `--clobber` uploads

## [0.3.0] — 2026-03-09

### Added
- **`create_project` MCP tool** — one-command colony setup with 8-phase onboarding prompt (CTO + adaptive worker profiles, auto/interactive modes, sprint planning)
- **`agent-relay update` CLI command** — self-update via GitHub Releases API (source build or prebuilt binary, launchd/systemd restart, `--force` flag)
- **Smart Messaging** — priority-based routing, conversations (group chats), delivery tracking, SSE real-time stream
- **Context budget pruning** — `get_inbox({ apply_budget: true })` scores messages by `0.7×priority + 0.2×tagRelevance + 0.1×freshness` and greedily selects the highest-value subset that fits the agent's byte budget. P0 messages always bypass the budget
- **Message orbs** — animated projectiles between agents on canvas (team, direct, broadcast)
- **Cancel button** on task notification cards — founder can reject agent tasks directly
- **Markdown rendering** in task notification cards (via marked.js)
- **Zoom controls** — +/- buttons and keyboard shortcuts for UI font scaling (localStorage persistent)
- **install.sh dependency audit** — checks curl (required), go, cc, git, jq, python3 with clear warnings
- **`.mcp.json` protection** — backup before merge, never overwrite existing config
- Auto-normalize JSON keys to snake_case
- Comprehensive MCP handler and REST API test coverage
- Reverse proxy docs, TLS troubleshooting, platform notes

### Fixed
- **Human task regression** — agents dispatching to `"human"` profile now trigger notification cards, kanban highlights, and My Tasks filter
- Repo URL corrected from `claude-agentic-relay` to `WRAI.TH` across all files
- Hook scripts guard for jq availability

### Changed
- `list_tasks` truncates descriptions/results to 200 chars (~70% token savings)
- `cancelled` status added to REST task transition endpoint

### Performance
- SQLite optimizations for concurrent agent workloads (WAL, busy timeout, connection pooling)

## [0.2.1] — 2026-03-08

### Changed
- Redesigned colony agent selection with Civilization-style macro→micro navigation

## [0.2.0] — 2026-03-08

### Added
- Opt-in authentication, CORS, rate limiting, body size limits

### Changed
- License switched from MIT to AGPL-3.0

## [0.1.1] — 2026-03-08

Initial public release — MCP relay server with SQLite persistence, canvas UI, pixel art galaxy/colony views, vault indexing, CI/CD with cross-platform binary builds.

[0.7.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.5.0...v0.7.0
[0.5.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/TsukumoHQ/WRAI.TH/releases/tag/v0.1.1
