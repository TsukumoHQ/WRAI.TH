# Changelog

All notable changes to wrai.th are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/). Versions follow [Semantic Versioning](https://semver.org/).

## [1.22.0] — 2026-09-25

Obligations become first-class relay data: the ACK checker now runs on a norms/obligations engine with an escalation chain agents can see, discharge or decline, questions sent to agents become tracked obligations that escalate when unanswered, while messages keep their task links and reply chains after retention, and memory writes stop losing concurrent updates.

### Added
- Obligation tools for agents: `obligations_mine` lists what you owe (your profile pool plus tasks assigned to you, with deadline and escalation depth), `obligation_discharge` marks one fulfilled after the relay re-checks it, and `obligation_decline` hands it up the chain immediately with a reason class (`not_mine`, `cannot`, `blocked_by`, `duplicate`) (task:79f48b9e, d1361bf)
- `send_message` takes an optional `task_id`, stored as the message's task link; an unknown id or one that contradicts `metadata.task_id` is refused before anything is written, and so is a malformed `reply_to`. A well-formed `reply_to` whose parent is missing is still accepted, and the result now reports `reply_to_resolved` (task:fde40c25, b8f3d88)
- Messages are linked to their task at insert: `messages.task_id` is filled from `metadata.task_id` or inherited from the `reply_to` parent in the same project, and older rows are backfilled on start (task:da1945d5, c2051b3)
- Message tombstones: the retention purge leaves one content-free row per deleted message (ids, routing, priority, no subject or body) in the same transaction, so a purged id can still be told apart from a mistyped one (task:f53b2160, c6add48)
- New `policy` message type for standing doctrine: it never expires, is never dead-lettered or purged, and is shown in its own `policies` block at every boot until the agent acks it (task:adefc1f4, 0571352)
- `set_memory` takes an optional `based_on` (the memory id you read, auto-filled from your last `get_memory`). If someone wrote a newer version in between, both stay live as siblings, the result names the conflict, and the other author is told (task:1a031d43, da972fc)
- Answer obligations: a direct or team message sent with `action_required` `ask` or `decide` now opens one obligation per recipient, fulfilled automatically when that recipient replies (the reply chain is followed through purged messages too). Recipients see them in `obligations_mine` and can discharge or decline them. Broadcast asks and conversation messages are excluded, and only messages sent after the upgrade are covered (task:044a4876, 038d089)
- Unanswered questions escalate: an answer obligation still open after `answer_reply_age` (1 h) is marked missed and passes to the recipient's manager (their `reports_to`, else an active executive), and after a further `answer_role_age` (2 h) to the human, who is only ever the last step. Each step sends one P1 message quoting the question, as a reply to it, so answering that message counts; a late answer from the original recipient closes the steps opened for it. The original message is never touched (task:a01d0b87, 5e48a6e)
- Boot selection journal: `get_session_context` logs one `[budget]` line per boot with the candidate, selected and omitted message ids, so the inbox budget can be replayed from logs. The boot payload itself is unchanged (task:409fb2e8, 6b3441c)

### Changed
- The ACK checker runs on a new norms/obligations engine instead of hard-coded checks. Behaviour is identical to v1.21, proven by an equivalence test against the old checker (task:f77efe18, 266a026)
- ACK escalation is now a chain: a task left unclaimed notifies its dispatcher at 15 min, escalates at 45 min, goes to the dispatcher's manager (else an executive, else the founder) at 90 min, and reaches the human operator only at 4 h. Notices are stored messages instead of push-only, so an offline dispatcher still gets them, and a notify can no longer arrive after an escalation. Tasks already escalated before the upgrade send nothing new (task:6b4369f0, d215a0c)
- Integrity scan: a slow scan log line now names the slowest phase and check, `orphan_agent_profile` only flags active agents, and a task on a missing board with no re-home target is recorded once instead of being logged on every sweep (task:27a77033, 52a9abf)

### Fixed
- A task promoted from backlog, unblocked or reset no longer fires the whole ACK chain at once: the ACK clock restarts each time a task becomes pending (new `tasks.pending_since`) instead of counting from its original dispatch (task:c933b2f1, c5288ca)
- The ACK clock also restarts when a task goes back to pending through a watchdog requeue, an expired lease, or the deactivation of the agent holding it (task:58ece5e2, cef0f87)
- The ACK sanction log line again ends with the task age in minutes, lost when the sanction code was shared with `obligation_decline` (task:904e024d, 81ba213)
- A memory writer working from an old version no longer silently archives a newer concurrent write; a superseded version now gets its `valid_until` closed, and re-saving the same value with a new layer is no longer dropped (task:df33d619, c4956f8)
- Replies to a purged message keep their parent's trace and action instead of waking the recipient as a new task, and `reply_to` checks treat a tombstoned parent as resolved (task:93b6f1cf, 5845c2f)
- Inbox budget scoring: an unknown priority no longer scores as P0, an unparseable or future `created_at` no longer counts as the freshest, and agents without tags can reach a full score. `apply_budget` is still off by default, so this was latent (task:1f190795, 8978751)
- Test suite: the token-usage flusher is stopped before a test closes its database, which removes a data race on captured logs. No change in production (task:d230b752, 4cbaa4c)

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

## [1.21.1] — 2026-09-09

### Fixed
- Messages view: a single **User** inbox groups messages addressed to `user` and `human`, for the current project or all projects, shown read-only

## [1.21.0] — 2026-09-08

One operator identity end to end for the console, plus the first triage of external pull requests.

### Changed
- Dependencies: mcp-go v0.55.1 to v1.0.0, go-sqlite3 v1.14.50, actions/setup-go v7 in the release workflow. Dependabot #147 and #146 as one bump, verified by a dedicated review of the mcp-go 1.0 surface (task:ad5035ab, c6fada1)

### Fixed
- Console identity, server half: messages sent from the console are stored as `from_agent = human`, the operator is reachable as `human` from any agent, and the forced-path and default cases are covered by tests (task:c5e67397, fecd12b)
- Console identity, console half: the v1 console and the v2 board, messages and memory views act as `human`, and the inbox accepts operator traffic addressed to `user` or `human` (task:ea0cc293, b559017)
- Fresh host: an ENOENT on the serve lock was misread as a second writer, so installs without systemd died right after the binary landed. External PR #149, rebased and gated (45090bf)
- `send_message` to an unknown recipient is rejected with a suggestion instead of silently accepted; the operator is exempt and a fleet-expected name that has not booted yet is queued rather than refused. External PR #152, rebased with three regression fixes (task:54ea39d8, ce9b270)

## [1.20.0] — 2026-09-07

Relay hygiene follow-ups (T2d) plus two founder-reported console fixes: send identity and the dyson picker alias.

### Changed
- `GET /api/settings` normalises duration values on the way out, so the flat view echoes what the store applied (T2d-c, 2f65ea3)
- Removed the dead `writableSettings` legacy 9-key panel allowlist (T2d-a, b24068e)
- Console: server-side alias `dyson_type <-> sun_type`: `GET /api/settings` carries `dyson_type` (auto when `sun_type` unset); `PUT {dyson_type:"off"}` sets `sun_type` to 0, `PUT {dyson_type:"auto"}` clears it; `sun_type` minimum is now 0 (d35ab8c)

### Fixed
- Console send identity: `POST /api/user-response` from the v1/v2 console now stores `from_agent = human` and delivers to the target agent, so the v1 human-profile filter matches messages sent from the UI (10571ea)
- Env-twin invalid values now WARN once per process instead of on every read (T2d-b, 4e2e833)

## [1.19.0] — 2026-09-07

Config-surface program T2 complete: one server-side settings spec drives the allowlist, validation, metadata and tick-time reads. No schema change.

### Added
- Settings spec table (`internal/relay/settings_spec.go`): every configuration key (console, linear, federation, server, operational, timing) has one spec entry with kind, source, env twin, bounds, default and note. The writable allowlist is derived from it (b4f0d7e)
- `GET /api/settings` returns per-key metadata: `groups` in fixed order plus a `settings` array with `key, group, kind, value, set, source, writable, secret, env_name, bounds, default, note`. Secrets never return a value, only `set` (b4f0d7e)
- `PUT /api/settings` validates the whole request before applying anything: `403 {error:"not writable", key}` for unknown or read-only keys, `400 {error:"invalid value", key, detail}` for kind, bounds or cross-key violations (`ack_escalate_age > ack_notify_age`, `deadletter_long_retention >= deadletter_short_retention`). Secrets: `""` = unchanged, JSON `null` = clear (b4f0d7e)
- Operational knobs read at tick time (`SettingDuration` / `SettingInt` with clamp, default and one WARN per key per process): `message_retention`, `audit_log_retention`, deadletter retentions, `token_usage_retention_days`, ack ages, `backup_keep`, `reviewer_ttl_days`, activity thresholds and more take effect on the next cleanup tick, no restart. Env twins (`RELAY_BACKUP_KEEP`, `RELAY_REVIEWER_TTL_DAYS`) keep precedence over stored settings. An empty settings table behaves byte-identically to the compiled constants (81339eb)

### Changed
- Console: the v2 settings panel is metadata-driven: six groups in server order, source badges `env` / `setting` / `default` / `compile-time (code)`, locked rows for read-only or env-sourced keys, inline server validation errors next to the offending key, secret rows as a set/unset pill with replace and clear, federation as a read-only summary linking to the federation page. v1 assets untouched (0156cfa)

## [1.18.0] — 2026-09-07

47 commits since v1.17.0: the single SQLite writer is no longer held by long maintenance work, input is typed and stricter, and projects gain an archive lifecycle.

### Added
- Project archive / unarchive lifecycle with `archived_at`, refuse-guards on archived projects, executive arm (9cb7e6d, 846c6b1, 373e328)
- Limbo sweeper: blocked > 7 d and stale tasks are archived on a schedule with flip-gate fixes; archive ONE task by id with reason and audit row (daf4fa7, 9da401c, 16909a1, 12c16ce)
- Dangling `board_id` re-home for native tasks (08c4a30); board-level Linear guard (09da35f)
- Self-grading guard: an agent cannot approve its own review (2ef3945)
- `orphan_profile` integrity class redefined; structured `integrity:` log lines (0717d89, e865264); scan covers triggers / memories / workflows (48deb93)
- REST routes: create-project, delete_agent, deactivate_agent (5c5cdca, 1af947d)

### Changed
- Typed validation for routing params (`as`, `project`, …): mistyped values are rejected instead of silently coerced (19bfa2e)
- `update_task` typed-field validation; it can no longer reassign `profile_slug` / `assigned_to` (89f3c94, 9e9ebd3)
- Refused mutating task calls are logged at INFO (75c9317)
- `ack_delivery` returns NOT_FOUND on a nonexistent delivery (54c40fa)
- `delete_memory` loud-fails on a non-matching targeted delete; executive admin arm can delete any memory (03b49b2, b072021)
- REST error bodies audited for a uniform shape (4127b08); `delivery_status` rows bounded by a `limit` arg (d121cf4)
- Tool-schema budget: headroom guard test + O1 schema reclaim, R1/R2 `session_context` trims (004e937, aa432ca, 6ba58f5, 63902c2)
- Registry listings hide `status='deleted'` rows (9dcd221)
- Console: v2 boards selector, archive-board action, delete-task button, project-selection "not found" state, config panel for the writable settings, `v1 console` back-link in the header (cfa9baa, 8958e42, 09c6516, f05484e, c92fda4, 4754bed)
- Console: v1 is now read-only (e292f83)
- CI: GitHub billed CI switched off; skill-gate review-verdict grep hardened (745a25a, b46647a)

### Fixed
- Writer starvation under load: relay writes could fail for up to 30 s while long maintenance work held the single SQLite writer. The referential-integrity scan now reads on the reader pool and applies in a short transaction (5163fb5)
- Writer-wait diagnostics: any single-writer wait ≥ 2 s is logged with holder and duration (bbf8003)
- Lowercase-at-write for profile slug, task assignee and message recipient, so lookups never need `LOWER()` (31dbbf7)
- Token-usage rollup aggregates off the writer and applies in a short transaction (ff5c908)
- `LOWER()` dropped from the scan's agent/profile lookups: indexed point lookups, idle scan from 2–4 s to under 2 s, plus a log-only case-invariant check (6cc5ccc)
- Linear reconcile loop no longer hammers Linear with a dead key (e9534c4)

### Upgrade notes
- No schema-breaking change: additive columns only (`tasks.verify_cmd`, `projects.archived_at`)

## [1.17.0] — 2026-09-02

Tasks can now carry an optional reviewer validation command, agent liveness can be read one agent at a time, and retried messages no longer wake the recipient twice.

### Added
- Tasks gain an optional `verify_cmd` field: a command the gate reviewer can run to check a ticket. `dispatch_task`, `batch_dispatch_tasks` and `update_task` accept it; `get_task`, `list_tasks` and `get_session_context` return it. It is never required, even on projects that enforce typed tickets, and the relay stores it verbatim without validating it. Changing it through `update_task` is dispatcher-only and audited, like `goal`/`acceptance_criteria`/`dod`. (task:866b13f2, 20c4f58)
- `GET /api/agents/health` accepts an optional `?agent=<name>` parameter that returns a single agent's health record (404 if the agent does not exist in the project). Every health record now includes `as_of`, the server clock at computation time, so callers can compute staleness without clock skew. Without `?agent=` the endpoint returns the same whole-project list as before. (task:6c1c5167, f2e731a)

### Changed
- The same duplicate-wake guard now covers the other message-creating paths: the REST message post (team and direct), federation inbound, cross-project send, `send_status`, and notification-rule messages. None of them pass an idempotency key today, so behaviour is unchanged; the guard prevents a double wake if one is added later. (task:1a7e18b1, be96fda)
- The REST user-response endpoint gets the same guard, closing the last of the seven call sites. No behaviour change today. (task:e60624d9, 809c9f5)

### Fixed
- A retried `send_message` that matches an existing `idempotency_key` no longer re-sends the live notification or the `message` activity event to the recipient. Before, the retry wrote no new message but still woke the recipient a second time. (task:cee47c61, b762173)

### Upgrade notes
- Schema: a nullable `verify_cmd TEXT` column is added to the `tasks` table automatically on startup. The migration is additive and idempotent.
- MCP tools: `dispatch_task`, `batch_dispatch_tasks` and `update_task` take a new optional `verify_cmd` parameter.

## [1.16.0] — 2026-09-01

### Added
- Phase 0 referential-integrity scan (detect+log+quarantine) (#164)
- Phase 1 referential-integrity reconcile (heal-resolvable, mark-rest) (#165)

## [1.15.0] — 2026-09-01

The Linear connector can mirror issues into several relay projects, `send_message` gains an idempotency key, opted-in projects get memory write rules, and a server hang caused by backups is fixed.

### Added
- New owner setting `linear_project_map` (JSON `{linearProjectId: relayProject}`) routes each Linear issue to a relay project chosen from its own Linear project, instead of writing every mirrored issue into the single project bound to the team. The web UI marks every relay project the connector writes to as a read-only mirror, not only the default one. (task:d1422119, fd937de)
- A `linear_project_map` value can now be a JSON array of relay projects, so one Linear project is mirrored into two or more relay projects. Only the first (primary) mirror writes status changes and comments back to Linear; the other copies are flagged with a new `tasks.linear_secondary` column and never write back. The follow-up commit makes the new tests race-free and fixes an unchecked `Close` error in `internal/db/db.go` that was failing lint. (task:0776e1a0, a2cbb25, 905596e)
- `send_message` accepts an optional `idempotency_key`. A resend with the same key from the same sender in the same project returns the original message and creates no new delivery, so a client that retries after a timeout no longer produces duplicate deliveries. Sends without a key behave as before, so heartbeats and repeated status messages are unaffected. (task:ac328091, 1d7509d)
- Memory write rules for projects that opt in through the new `projects.require_memory_discipline` flag: `set_memory` rejects checkpoint, resume or date-stamped keys, requires at least one tag, and requires `valid_until` no more than 7 days out when `layer=context`; values over 600 characters are accepted with a `warning` in the response; `remember` rejects `area="general"`. Each rejection includes a hint. The REST memory POST is gated the same way. Existing memories are not touched. (task:4e02d199, bdd0717)

### Fixed
- The relay could stop answering HTTP while staying up: the database backup ran `VACUUM INTO` on the single writer connection, so a slow backup blocked every write with no timeout. Backups now use their own read-only connection, and every writer call is bounded by a 15-second timeout that aborts a stuck query instead of hanging. The server also now opens its port and logs `listening on ...` before database init and migrations run, so a slow startup on a large database is visible rather than looking like a hang. (task:34037526, 2ade5fe)

### Upgrade notes
- Schema (added automatically on startup, additive): `messages.idempotency_key` (with a partial index `idx_messages_idempotency`), `tasks.linear_secondary` (default 0, so existing single-target mirrors stay primary and keep writing back), and `projects.require_memory_discipline` (default 0).
- `require_memory_discipline` is switched on once, at first start, for the `niwa` and `tsukumo` projects; an operator's later opt-out survives restarts. All other projects are unaffected unless they opt in. On an opted-in project, REST `/api/memories` writes with `layer=context` are always rejected, because the REST body has no `valid_until` field.
- New setting `linear_project_map`; its values may be a string or a JSON array of relay project names.
- MCP tools: `send_message` has a new optional `idempotency_key` parameter.

## [1.14.0] — 2026-08-26

### Fixed
- Linear: carry profile_slug (routing lane) onto Linear mirror tasks (#160)

## [1.13.0] — 2026-08-22

Tasks and messages get a `trace_id` for following one dispatch end to end, and several task-creation and update paths that silently dropped or misplaced input now refuse or record it.

### Added
- `trace_id` correlation id (32 lowercase hex) on tasks, messages and audit entries. `dispatch_task` mints one when none is given, a subtask inherits its parent's, and a caller can pass its own through a new optional `trace_id` parameter (a malformed value is refused). Replies inherit the parent message's id, task-announcement messages inherit the task's, and audit entries for a task inherit the task's. `get_task` and the `task.dispatched` event carry it. Limits in this version: only the MCP `dispatch_task` tool accepts a caller-supplied id, message reads do not return it yet, and lifecycle events other than dispatch do not carry it. (task:48ee1d94, 5eae07f)

### Changed
- On a project with more than one board, a dispatch without `board_id` is now refused with a message listing each board as `slug (id)`, instead of landing silently on the oldest board. This applies to `dispatch_task`, `batch_dispatch_tasks` (per item), REST dispatch, cron schedules and inbound-signal webhooks. Projects with zero or one board behave as before. `dispatch_task` also accepts a board slug as `board_id`, and refuses a `priority` that is not one of the strings `P0`-`P3` instead of quietly using `P2`. (task:30a455fc, a0de115)
- `register_agent`'s `cwd` description, the onboarding prompt and the skill doc written by `agent-relay init` no longer say "one agent per worktree", matching the several-agents-per-directory behaviour. A failed working-directory bind at registration is now logged instead of silently ignored. (task:4bd9cb95, 298a3e5)
- The `set_run` conflict error no longer claims `run_state` changed, since a concurrent delete or archive gives the same result; it tells the caller to re-fetch and retry. A new concurrency test goes through the public `SetTaskRun` so removing the guard would fail CI. (task:deebb85c, 3a3ec48)
- Maintenance: trimmed the `dispatch_task` description to bring the tool-schema size test back under its budget; no behaviour change. (task:d7080bc2, d4ca7b4)

### Fixed
- `update_task` silently ignored `goal`, `acceptance_criteria` and `dod`. It now writes them, only when called by the task's dispatcher (the assignee is refused), checks that `acceptance_criteria` is a JSON array, and records the old and new values as a `contract_updated` audit entry. (task:c229fe44, 8e65593)
- `send_status` dropped `done`/`doing`/`blockers` values that were not a list of strings (for example a plain sentence or a number). Such input is now appended to the status note instead of being lost; well-formed calls are unchanged. (task:d601ac41, edf6703)
- `set_run` could overwrite a concurrent `run_state` change; it now fails with `RUN_STATE_CONFLICT` instead. The unacknowledged-task scanner no longer nags or escalates run containers (parent tasks that stay `pending` by design), and it no longer notifies for a task that was claimed, turned into a run container or already notified between its read and its write. (task:5972890c, 9cc8d8a)

### Upgrade notes
- Schema (added automatically on startup, additive, nullable): `trace_id TEXT` on `tasks`, `messages` and `audit_log`.
- MCP tools: `dispatch_task` gains an optional `trace_id` parameter; `update_task` gains `goal`, `acceptance_criteria` and `dod` (dispatcher only).
- Behaviour: on multi-board projects, callers that omit `board_id` (including operator-written cron schedules and signal webhooks without a board) now get an error until a board is named.

## [1.12.0] — 2026-08-21

v1.12.0 links tasks to GitHub PRs and keeps them in sync, lets senders mark messages as no-wake, adds scheduled, webhook-created and backlog tickets, recovers stuck work automatically, and moves token telemetry out of the coordination database.

### Added
- New `delivery_status` tool shows where each delivery of a message stands (queued, surfaced, acknowledged, expired), filtered by `message_id` or recipient. `get_thread`, `get_message` and `get_team_inbox` now include the caller's `delivery_id` so a message can be acked from those reads, and the session context includes a `cross_project_unread` count of unread and P0 messages per other project. `set_memory` now returns the stored, normalized validity window instead of echoing the raw input (task:6eaa24b0, 70132bd)
- New `identity_check` tool reports whether an agent name can be woken: registered or not, cwd bound or not (a "ghost"), and which other active agents share its cwd (task:c0432119, b32257f)
- New `deadletter` table and `deadletter` tool: when an unread message expires on its TTL, a record of it (sender, recipient, priority, subject, expiry time) is kept even after message retention deletes the message itself (task:8b097685, db73e99)
- Memories returned by the memory list and search tools now carry a derived `importance` score between 0 and 1, computed at read time from layer, confidence, recency and version. No schema change; weights can be tuned with `RELAY_MEM_W_*` environment variables (task:743520ff, 2472901)
- `search_memory` accepts `rank="mempalace"` to re-order full-text results by a mix of relevance, recency and importance, returning a `rank_score`. The default bm25 order is unchanged (task:4bc7ebc1, c9ccdfb)
- The relay now serves read-only MCP resources `relay://tasks`, `relay://agents`, `relay://boards` and `relay://memory`, compact indexes scoped to the connection's project, so agents can see the board, roster and memory keys at connect time without a tool call (task:5d10b7d4, 79efa20)
- New `link_pr` tool stores a GitHub PR on a task (`pr_url`, `pr_number`, `pr_repo`, `pr_state`); omitted fields keep their current value. The `relay://tasks` resource shows the linked PR (task:d71ca43d, 44b77f1)
- Incoming GitHub `pull_request` webhooks now move the linked task: opened, reopened or ready for review goes to in-review, merged goes to done, closed without merge goes to blocked. The task is found by its stored PR or by a `relay-task: <id>` line in the PR body. Done or cancelled tasks are never moved back (task:58966fcd, b8767f8)
- New `relay://pr-reconcile` resource lists tasks whose linked PR is still open while the task is not finished, so an external poller can catch a missed webhook (task:001ecf94, 0997f9d)
- New `reconcile_pr` tool lets that poller report a PR's observed state; the relay records it and applies the same status mapping as the webhook, returning whether the task changed (task:16bb7386, ce9c5e3)
- `GET /api/conversations` and `GET /api/messages` accept `?user=<name>` to return only that user's conversations (active membership) or messages (sender or recipient). The filter runs before the row limit and matches names case-insensitively (task:6da11b65, d6ce0a6)
- New `set_run` and `get_run` tools group a multi-agent run under its parent task: `set_run` records an `integration_branch` and moves `run_state` through open, gating, merging, merged, blocked or amputated (transitions enforced, merged is final); `get_run` returns the parent and its subtasks. A task with `run_state` set is a container and cannot be claimed or started (task:6641e3ad, 768d348)
- `send_message` takes an optional `action_required` (`ask`, `do`, `decide`, `none`). If omitted, the relay derives it from the message type. `none` messages stay in the inbox but no longer count toward the wake signal; P0, questions and tasks always wake whatever the tag, and older rows with no tag still wake (task:a51a784b, f1bf01e)
- New read-only `GET /api/metrics/comms?project=&hours=` reports the no-wake share against a 0.55 target, `action_required` usage per sender, read-latency buckets and messages that could have been combined into one wake (task:bd83c2a5, 35c367d)
- `GET /api/metrics/comms` also reports message length (average, p50, p90, sample count) for the same project and window (task:cef6a890, fbe8654)
- New `send_status` tool posts a structured status (`done`, `doing`, `blockers`, `note`) that never wakes the recipient unless sent as P0. Items over the size limits are truncated and flagged, not rejected (task:2b00cd82, 18509e0)
- New `POST /api/webhooks/signal` turns a signed external event (CI failure, error alert) into a task dispatched through the normal pipeline. It needs `RELAY_SIGNAL_WEBHOOK_SECRET` (returns 503 without it) and a `signal_source:<source>` setting that picks the profile, so the payload cannot choose who gets the work (returns 403 without it). Redeliveries with the same `X-Signal-Delivery` id do not create a second task (task:d58b522e, 6b4140c)
- New `GET /api/agents/stuck` lists agents that have gone quiet (default 30 minutes) while still holding an accepted, in-progress or in-review task, and `POST /api/tasks/{id}/requeue` puts such a task back to pending with its lease cleared. The relay only exposes these; the host daemon decides what to kill (task:05fc4373, 79dbea4)
- Decisions saved with `remember` accept `depends_on` links to other decisions. New `GET /api/decisions/graph` renders the decision history as a Mermaid graph, and `GET /api/decisions/relevant?area=` returns the live decisions for an area (task:b3746438, 744f94b)
- Scheduled tickets: tasks listed in the `cron_schedules` setting (6-field cron expressions, minute granularity) are dispatched on schedule through the normal dispatch pipeline, at most once per matching minute, including across restarts. Nothing runs until the setting is filled in (task:4cee49b6, cedce0e)
- A background sweep every 2 minutes moves tasks whose linked PR is already recorded as merged or closed to done or blocked, and returns tasks to pending when their lease has expired and the holder is no longer live, then nudges the profile. A live holder with an expired lease is left alone (task:b3d061a6, ea362ce)
- New `backlog` task status: `dispatch_task` with `backlog=true` creates a task that is visible but cannot be claimed and wakes nobody, and the new `promote_task` tool moves it to pending and announces it like a fresh dispatch (task:121f0ff5, 313c1c3)

### Changed
- Tool errors now return one JSON shape with `code`, `errorCategory` (`transient`, `validation` or `permission`), `isRetryable` and `message`. Existing specific codes such as `SENDER_INACTIVE` and `TASK_LEASE_HELD` are kept (task:06e65fd2, a62a0de)
- P0 messages no longer expire, whatever TTL the sender gave, and P0 messages already stored with an elapsed TTL show in the inbox again. P1 messages without an explicit TTL now default to 7 days; P2 and P3 keep 4 hours (task:bbb81e0a, 1583e1c)
- `register_agent` now refuses an empty name, the name `anonymous`, and the `default` project with the code `ANONYMOUS_REGISTRATION_REFUSED`, before writing anything (task:24ec2c96, bef6243)
- The `deadletter` table is cleaned up by the 5-minute cleanup loop: records are deleted after 30 days, or after 180 days for P0 and P1 (task:aa3c53fe, cb252ef)
- Several agents can now share one cwd, as teams on a shared worktree do. Registering no longer clears another agent's cwd binding; `register_agent` lists the others in `cwd_shared_with`, and `identity_check` treats a shared cwd as normal (task:8362f5f2, da6b094)
- Typed-ticket enforcement (refusing a task without goal, acceptance criteria and definition of done on projects that require them) now covers every task-creation path, including scheduled and webhook-created tasks. It is checked before any board or profile is auto-created, and it is switched on once at boot for the `tsukumo` project as well as `niwa`. The signal webhook answers 422 for a refused ticket (task:b092a6be, 7f47ee8, 1b5e533, 717692e)
- The cron scheduler only dispatches after reading back its once-per-minute marker; if the marker was not saved it skips that run instead of risking a duplicate task (task:553f17ce, 46014d5)
- Token-usage telemetry moved out of the coordination database into a separate `<db>.analytics.db` file, so backups and checkpoints no longer copy it. Raw rows are kept 14 days instead of 30, and a new `token_usage_daily` table keeps daily totals beyond that (task:de1ee790, 52be273)
- `get_inbox` no longer counts the whole deliveries table on every call, and the fallback inbox query uses a new `(project, to_agent, created_at)` index instead of scanning every message in the project (task:1dc3be08, 63e1e8a)
- Marking inbox messages as surfaced now takes one database write per batch instead of one per message, and session lookups only scan the agents of the current project (task:bb541360, 6fa928d)
- Two new indexes on `tasks` speed up the PR sweep and the unacknowledged-task check, and the agent stats query now reads only the columns it needs, capped at the 20,000 newest tasks (task:1b89a212, 90f78ad)
- Maintenance: stuck-agent documentation and tests now match the actual behaviour (inactive agents are included; only sleeping and deleted ones are skipped); added a test for the cron marker read-back; `.mcp.json` is no longer tracked and ships as `.mcp.json.example` (task:b69645cb, 964560b; task:1ec6383d, 8dfacc9; task:c74b5c4c, 04564a0)

### Fixed
- Reassigning a task now updates its `profile_slug` to the new assignee's profile, and a one-time migration corrects tasks that were already reassigned (task:4201ad5c, cf5a7b8)
- The migration that deletes the old `default` project no longer stops on a "FOREIGN KEY constraint failed" error. It now also removes the conversation members, conversation reads, team inbox and message reads rows linked to that project, and rebuilds the memory and vault search indexes (task:52f7502d, f3b7027)
- Project names are now normalized (trimmed, lower-cased, `_` replaced by `-`) inside the project registry, so `create_project` with an underscore or mixed case no longer creates a separate project nobody can reach, and deleting a project by a differently-cased name now works (task:0f2d5c3e, 4cffe82)
- A dispatch now reaches workers that were marked inactive after 30 minutes without relay calls, and leaves a queued delivery that can wake them. An agent holding an unexpired task lease is no longer marked inactive (task:6509668c, 5b760a1)
- Deleting a task now also deletes its progress notes; subtasks and messages about the task are kept (task:d21e7ef6, 34203d6)
- Notification rules: escalations to "manager" fall back to the task's dispatcher when the agent has no `reports_to`; "dispatcher" and "owner" targets now resolve; blocked-task escalations wake the recipient again while task-done notices do not; notifications that render empty are skipped instead of sent (task:9990c3f9, 930ed3a)
- The signal webhook now enforces the project task quota (429 when over), and if creating the task fails it frees the delivery id so the sender's retry is not treated as a duplicate and lost (task:ce8f4ba4, 35138dc)
- The daily token-usage rollup no longer undercounts the day at the edge of the 14-day raw retention window (task:48fb2646, c8c5a28)
- `send_message` to an agent that has not registered yet but has already been dispatched a task under that name now queues the message instead of refusing it; the message appears once the agent registers. Names never registered and never dispatched are still refused (task:0464d6cb, d80fc1a)

### Upgrade notes
- Schema changes are applied automatically at boot and only add things: new `deadletter` table (index `idx_deadletter_agent`); new `tasks` columns `pr_url`, `pr_number`, `pr_state`, `pr_repo`, `integration_branch`, `run_state`; new `messages` column `action_required`; new indexes `idx_messages_project_to`, `idx_tasks_pr`, `idx_tasks_status_dispatched`.
- First boot moves `token_usage` into a new file next to the database (for `relay.db` it is `relay.analytics.db`), attached as schema `analytics`, which also holds the new `token_usage_daily` table. The old table is copied, dropped, and the coordination database is VACUUMed once, which can take a while on a large database. Hourly backups no longer include token usage, so back up the analytics file separately if you need it. Raw token rows are now kept 14 days.
- New MCP tools: `delivery_status`, `identity_check`, `deadletter`, `link_pr`, `reconcile_pr`, `set_run`, `get_run`, `send_status`, `promote_task`. New parameters: `send_message.action_required`, `search_memory.rank`, `remember.depends_on`, `dispatch_task.backlog`. New MCP resources: `relay://tasks`, `relay://agents`, `relay://boards`, `relay://memory`, `relay://pr-reconcile`.
- New HTTP endpoints: `POST /api/webhooks/signal`, `GET /api/agents/stuck`, `POST /api/tasks/{id}/requeue`, `GET /api/decisions/graph`, `GET /api/decisions/relevant`, `GET /api/metrics/comms`, and a `?user=` filter on `GET /api/conversations` and `GET /api/messages`.
- New optional settings and environment variables, all off or at defaults until set: `cron_schedules` (scheduled tickets), `signal_source:<source>` plus `RELAY_SIGNAL_WEBHOOK_SECRET` (signal webhook), `RELAY_MEM_W_LAYER`, `RELAY_MEM_W_CONFIDENCE`, `RELAY_MEM_W_RECENCY`, `RELAY_MEM_W_VERSION`, `RELAY_MEM_RECENCY_HALFLIFE_HOURS`, `RELAY_MEM_RANK_W_RELEVANCE`, `RELAY_MEM_RANK_W_RECENCY`, `RELAY_MEM_RANK_W_IMPORTANCE` (memory scoring weights).
- One-time boot actions, recorded under the settings keys `seed_tsukumo_typed_ticket` and `backfill_task_profile_slug`: typed tickets are switched on for the `tsukumo` project, and stale task `profile_slug` values are corrected. Turning typed tickets off later for `tsukumo` is not undone by a restart.
- Behaviour to check before upgrading: tool errors are now JSON objects, so clients that match on error text should read `code` instead; `register_agent` refuses empty or `anonymous` names and the `default` project; P0 messages never expire; messages tagged or derived as `action_required=none` (including notification and status types) no longer wake the recipient.

## [1.11.0] — 2026-08-18

Task ownership becomes an atomic lease that a supervisor can take back from a dead agent, inactive agents are refused when they send, and memories can expire and keep a record of why they were archived.

### Added
- Task leases: claiming, starting or submitting a task for review makes the working agent the lease holder for 2 hours, and each further transition by that agent extends it. Completing, blocking or cancelling releases it; an orchestrator reassign moves it to the new agent. A new `reclaim_task` tool takes over a task whose holder's lease has expired or whose holder is deregistered or inactive; it refuses a live holder's task with `TASK_LEASE_HELD`. A claim that loses a race now gets a structured `TASK_STATE_CONFLICT` error instead of a plain string, so a client can stop rather than retry in a loop. Lease hand-offs emit a `task.lease_transferred` event (from, to, reason) and are written to the audit log. (task:6d055fb2, 134a7d0)
- Sender liveness check: `send_message` and `ack_delivery` now refuse an unregistered, inactive or deleted sender with a structured `SENDER_INACTIVE` error that includes the reason. `register_agent` takes a new `is_service` flag for monitoring or QA daemons, which stay allowed to send even when every worker is down; the flag is kept when omitted on re-registration. A new read-only `is_eligible` tool returns `{eligible, reason}` for an agent without sending anything. (task:160c66ad, bcf1154)
- Memory validity windows: `set_memory` accepts `valid_until` (and `valid_from`); past `valid_until` a memory is reported as `stale` rather than deleted. `search_memory` and `list_memories` show a `status` of `live`, `stale` or `archived`, and hide stale memories unless `include_stale=true`. `delete_memory` takes an optional `reason` stored with who and when, and a superseded decision is archived with reason `superseded`. `get_memory` on a key that has no live value now returns its archived record instead of an empty result. (task:662d17a4, a6aa5a0)

### Upgrade notes
- Schema (applied automatically on startup, additive): `tasks.lease_holder`, `tasks.lease_expires_at`, `tasks.lease_heartbeat_at`; `agents.is_service` (default 0); `memories.valid_from`, `memories.valid_until`, `memories.status` (default `live`) and `memories.archived_reason`. Existing memories are backfilled with `valid_from = created_at`, and already-archived rows get `status = 'archived'`.
- New MCP tools: `reclaim_task`, `is_eligible`. New parameters: `register_agent` `is_service`; `set_memory` `valid_until`; `search_memory` and `list_memories` `include_stale`; `delete_memory` `reason`.
- Behaviour: a daemon or script that sends messages under an identity that is not registered and active will now get `SENDER_INACTIVE`; register it (with `is_service=true` for monitoring or QA identities).

## [1.10.0] — 2026-08-17

### Added
- Structured logging via slog + RELAY_LOG_LEVEL (#155)
- Get_message tool: full untruncated body by id (WRAITH-3) (#156)

### Changed
- CI: skip cross-compiler apt install on native linux/amd64 (#159)

### Fixed
- Wake-count counts only new deliveries; mark_read acks conversations (WRAITH-2) (#157)
- Cap unbounded session_context sections (WRAITH-1) (#158)

## [1.9.0] — 2026-08-17

### Added
- Kill anonymous/default fallbacks + identity binding (v2) (#151)

### Fixed
- Session rebind ambiguity (#153), delete_team (#150), ar init stdio (#20), unread HTTP endpoint (#17) (#154)

## [1.8.0] — 2026-08-05

Dispatches can carry a typed ticket (goal, acceptance criteria, definition of done) that projects can require, project names are normalized so one project no longer splits into several, and message events tell a wake daemon enough to decide without reading the inbox.

### Added
- Typed tickets: `dispatch_task`, `batch_dispatch_tasks` and REST `POST /api/tasks` accept `goal`, `acceptance_criteria` (a JSON array) and `dod`, stored on the task and returned by `get_task`/`list_tasks`. A project with the new `require_typed_ticket` flag refuses a dispatch missing any of them, naming the missing fields; batches apply the rule per item. The flag is off by default and switched on for `niwa`. Documented in `docs/typed-tickets.md`. (task:f48b827d, 669b79c)
- Linear issues are held to the same typed-ticket rule: the ticket is read from `## Goal`, `## Acceptance Criteria` and `## DoD` sections in the issue description. On an enforcing project a conforming issue is mirrored with those fields filled in, and a non-conforming one is not dispatched and gets a comment on the Linear issue naming the missing sections. Tasks already in progress are never refused afterwards. (task:60110aa5, 8d6e35f)
- A non-conforming Linear issue is now stored as a `refused` task that is never dispatched, and the Linear comment is posted only once whether the issue arrives by webhook or by the reconcile poll. If the issue is fixed it leaves `refused` and dispatches normally; if it later becomes non-conforming again before work starts, it is commented on once more. (task:6dd7f48e, 4bb9730)
- The SSE `message` event now carries the message `priority`, on the MCP send path and both REST send paths, so a wake daemon can tell urgent messages apart without fetching the inbox. (9dfa56a)
- The SSE `message` event also carries `msg_type` (for example notification, ack, fyi), including on federated messages, so a wake daemon can skip waking a busy agent for an ack or fyi. (task:048c478e, 4c35901)
- `register_agent` and `create_project` reject malformed project names (empty, path-like, starting with a dot such as `.agentd`, or over 64 characters); one `@` is allowed for `name@host` remote projects. `register_agent` is rate-limited per project and agent name (one call per 5 seconds, bursts of 5) to stop runaway re-registration loops. (30f2254)

### Changed
- Project names are normalized (trimmed, lowercased, `_` turned into `-`) on every tool parameter, MCP session, REST `?project=` and `target_project`, so `synergix_prod` and `synergix-prod` are the same project. A call with no project now uses the caller's own registration when it belongs to exactly one project, instead of falling into `default`. `send_message` to a project that does not exist now fails and suggests the closest existing name, instead of storing the message where nobody reads it; `default` is still accepted. (497bf05)
- Cycle-digest notifications stop for boards with no task activity within two digest intervals of the last one; new task activity turns them back on. Retired projects no longer post a `Cycle …` message to the human every interval. (71b822c)
- Maintenance: added the `review-ai-skills`, `review-installation` and `review-agent-runtime` review skills under `.claude/skills/`, and ignored the local worktrees directory in `.gitignore`. (cf072b0, f057930, 38abcbf, bbd3064)

### Fixed
- An upgraded relay opening an older database would split existing projects whose stored names were not in normalized form (an agent registered as `testDuSoir` became invisible to lookups for `testdusoir`). A boot-time migration now rewrites the project name in every table to the normalized form; when both spellings already exist it keeps the normalized row and drops the unreachable duplicate. Internal `_`-prefixed projects are left alone. (995b673)
- The `niwa` typed-ticket default was re-applied on every boot, silently undoing an operator's opt-out. It is now set once, marked by the `seed_niwa_typed_ticket` setting, and later changes survive restarts. (task:cee0b4bd, a2a51c6)

### Upgrade notes
- On first start, stored project names in every table are rewritten to lowercase with `-` instead of `_`. Duplicate rows that differ only by spelling are merged into the normalized one. Clients and scripts that use mixed-case or underscore project names keep working, because incoming names are normalized too.
- Schema (added automatically, additive): `tasks.goal`, `tasks.acceptance_criteria` (default `[]`), `tasks.dod`, `tasks.refusal_notified_at`, `projects.require_typed_ticket` (default 0). A `niwa` project row is created if missing and has typed tickets switched on once.
- New task status value `refused` for Linear mirrors that fail the typed-ticket check.
- Behaviour: `send_message` to an unknown project now returns an error; `register_agent` is rate-limited and rejects malformed project names.

## [1.7.2] — 2026-07-29

### Added
- Tasks: git zone + enriched in_review event for external review gates (#148)

## [1.7.1] — 2026-07-01

### Added
- Federation config via settings + dashboard UI panel (#144)

## [1.7.0] — 2026-07-01

### Added
- Lead-machine autonomy notification rules (TSU-146) (#136)
- Metrics: dead-lettered event count in /api/metrics (TSU-147) (#137)
- Cost: per-day $ rollup /cost/daily (TSU-153) (#138)
- Federation: relay-to-relay direct messaging between peers (#143)

### Changed
- CI: publish-gate poll 120s to 12min (fix skipped release announce) (#139)
- Docs: honesty + quickstart sweep for the OSS launch (TSU-260) (#141)
- Dependencies: bump the go-deps group with 2 updates (#124)

### Fixed
- Linear: done-dropout sync: close mirrors for issues that left the open poll (TSU-159) (#140)
- Autonomy rule fires on event:lead-ready (match live emitter) (#142)

## [1.6.0] — 2026-06-30

### Added
- Installer: ship a public end-user Claude Code skill (TSU-223) (#133)
- Installer: clean-room install gate for wrai.th (TSU-224) (#134)

### Fixed
- Rewrite create_project onboarding to the current model (TSU-207) (#131)
- Installer: honest post-install summary: don't claim 'relay is running' (TSU-223) (#132)
- Linear: stop reconcile resurrecting terminal mirror tasks: phantom-stale (TSU-159) (#135)

## [1.5.0] — 2026-06-28

### Added
- DB integrity check + verified restore drill (TSU-137) (#129)
- GET /api/metrics ops snapshot for monitoring (TSU-141) (#130)

### Changed
- Tests: concurrent-agent soak + perf baseline (TSU-134) (#128)

### Fixed
- Linear: reconcile poll dispatches delegate-assigned issues in unrouted projects (TSU-117) (#125)
- Linear: route by Issue.delegate (guarded, fallback to assignee) (TSU-121) (#126)
- Message + audit_log retention GC: prevent unbounded table growth (TSU-127) (#127)

## [1.4.9] — 2026-06-28

### Changed
- Custom `event:*` deliveries carry the full payload (JSON) into message content and meta instead of flattening it (#122)

### Fixed
- Linear to agent auto-dispatch: a routed Linear issue moved to In Progress now fires the P0 to the assigned agent (dispatchEvent emits `task.dispatched`, matching the auto-claim rule). Previously routed issues mirrored to the board but never pinged the agent (#123)

## [1.4.8] — 2026-06-28

### Added
- Reply-path grant (TSU-75): receiving a DM grants a scoped, time-boxed path to reply back across the auth wall, so answering a P0 sent to you needs no detour (#121)

### Fixed
- Non-destructive reads (TSU-73): `get_inbox` no longer marks read on fetch or truncates a P0; `mark_read` is explicit and the first read returns full content. Stops silent message loss (#120)

## [1.4.7] — 2026-06-27

### Fixed
- Atomic binary swap (write `.new` + rename, never in-place truncate): a relay self-update no longer SIGBUSes the live stdio MCP pipe (TSU-74, #119)
- Staged restart: `restartService` polls the port free before bootstrap (no EADDRINUSE to fatal dead-relay race); `--no-restart` / `--stage` for auto-trigger safety (TSU-74, #119)

## [1.4.6] — 2026-06-27

### Added
- Console: fleet console default view + constellation behind Show mode (#114)

## [1.4.5] — 2026-06-27

### Fixed
- Activity: live state stream: join by agent name, not stale session_id (#113)

## [1.4.4] — 2026-06-27

### Fixed
- Hooks: stop hook sends model to /ingest/tokens (per-tier cost) (#112)

## [1.4.3] — 2026-06-26

### Changed
- Docs: marketing layer: add dokan as the 4th suite pillar (#103)

### Fixed
- Backup: widen DB snapshot retention 3 to 12 (12h recovery window) (#110)
- Tokens: register_agent must pass cwd: closes token attribution (TSU-64) (#111)

## [1.4.2] — 2026-06-26

### Added
- Observability: TSU-53 slice-C: budget-exceeded alert (relay acts on the budget) (#106)
- Observability: budget alert to cto (P1) + quota API for the runaway tripwire (#107)
- Observability: TSU-53 slice-D: cost + health card on the stats board (#108)

### Fixed
- Server: single-writer lock: two relays on one DB is now impossible (#109)

## [1.4.1] — 2026-06-26

### Fixed
- CLI: init upgrades existing .mcp.json URL to ?tools=full (#104)
- Tools: onboarding core in discovery mode (keep token economy, no ?tools=full default) (#105)

## [1.4.0] — 2026-06-26

### Added
- Hook-POST ingestion: real tokens, cwd identity, lock-free (#72)
- Console board: radial hub-and-spoke org layout (#73)
- Console board: team-grid layout, readable transcript, conversations, corporate type (#74)
- Linear: project to agent auto-dispatch routing (configurable backend) (#75)
- Linear: reconcile polls all open team issues (not just active cycle) (#76)
- Linear: relay to Linear backfill (one-shot, dry-run-safe) (#77)
- Notifications: assignee/dispatcher dynamic rule targets (#78)
- Host relay-send forwarder + loopback /api/send (#80)
- Notifications: resolve assignee from routed lane for Linear-sourced tasks (TSU-52 unblock) (#85)
- Notifications: set-membership match: scope stale-rule to active tasks (#86)
- Events: TSU-52 slice-A: durable outbox + replay log (#87)
- Events: TSU-52 slice-B: outbox sweeper drives delivery (durable + DLQ) (#88)
- Events: TSU-52 slice-D: GitHub webhook receiver (HMAC) into the outbox (#89)
- Tasks: last_activity_at: stale clock resets on activity (calibration) (#90)
- Memory: TSU-51 slice-A: remember verb (ADR-style decision log) (#91)
- Memory: TSU-51 slice-B: inject accepted decisions at session start (#92)
- Observability: TSU-53 slice-A: per-agent $ cost rollup (per-tier, cache-aware) (#94)
- API: plain-REST send endpoint POST /api/messages (off-MCP notifiers) (#96)
- Observability: TSU-53 slice-B: per-agent health badge (last_seen x token-delta) (#98)
- CLI: agent-relay hooks install/status: reliable hook onboarding (#99)

### Changed
- Reverted: tear down forwarder (PR #80): dokan reaches relay direct (#81)
- Docs: teach the autonomy subsystems + architecture diagrams (#97)
- Installer: delegate hook setup to 'agent-relay hooks install' (#100)

### Fixed
- Linear: poll only mirrors issues in a routed project (#79)
- Branding: purge Synergix-lab to TsukumoHQ/WRAI.TH (last live public leak + broken install/auto-update) (#82)
- Routing: skill to profile dispatch dead from column/scan mismatch (#83)
- Tasks: close transitionTask TOCTOU: CAS on status guards double-claim (#84)
- Stale detection: Linear dispatch transition bumps last_activity_at (#93)
- Observability: cost falls back to bytes/4 estimate for legacy token rows (#95)
- CLI: hooks install refuses on Windows until native .ps1 hooks land (#101)
- Tools: init ?tools=full + skill/hook refresh on update (create_project not found) (#102)

## [1.3.2] — 2026-06-24

### Changed
- Dependencies: bump golangci/golangci-lint-action from 7 to 9 (#41)
- Dependencies: bump actions/upload-artifact from 4 to 7 (#42)
- Dependencies: bump actions/download-artifact from 4 to 8 (#43)
- Dependencies: bump actions/checkout from 4 to 7 (#68)

## [1.3.1] — 2026-06-24

### Added
- MCP: 'agent-relay mcp' stdio transport (registry/.mcpb prerequisite) (#70)

### Changed
- Dependencies: bump actions/setup-go from 5 to 6 (#40)
- Dependencies: bump the go-deps group across 1 directory with 3 updates (#69)
- Docs: cross-property suite section + llms.txt (AEO/entity) (#64)

### Fixed
- Server: exit on bind failure instead of hanging as a zombie (#71)

## [1.3.0] — 2026-06-19

### Added
- Console (v2): rebuild mission control as a fleet-ops console (#62)

### Changed
- Docs: low-key tsukumo consulting bridge (UTM'd) (#66)

### Fixed
- Security: harden relay defaults: 1 MiB body cap + opt-in identity gate (#67)

## [1.2.0] — 2026-06-15

### Added
- Linear: status + comment write-back to Linear (the only two agent actions) (#60)

### Fixed
- DB: return newest messages first for Messages view (#59)
- Console (v2): SSE stream stuck on "connecting" for idle projects (#61)

## [1.1.0] — 2026-06-15

### Added
- Console (v2): command layer: dependencies, force-transition, audit trail (#48)
- Console (v2): messages comms desk + memory curation (#50)
- Console board: filters, group-by, staleness + overload (#55)

### Changed
- Perf+fix: token efficiency (discovery default, Def.7 ceiling, lean thread/team inbox) + EventBus race (#29)
- Dependencies: bump go-sqlite3 to v1.14.45 (clears 2 CVEs) + prune dead spawn-era deps (#31)
- MCP: clamp caller limit + fix stale get_profile desc (token P2) (#32)
- Split Handlers god-object into per-domain files (#33)
- CI: .gitattributes + pin golangci + harden release create (#34)
- Dependencies: bump mcp-go + x/sys, add Dependabot + govulncheck (#38)
- Docs: update README for v1.0 (main feature set) (#51)
- Untrack notebooks/ (paper under revision) (#52)
- Clean up stale repo files (#53)
- Remove scripts/team-saas.sh (demo-saas gone) (#54)
- Docs: pivot README to v2 dashboard + real screenshots (#56)
- Docs: add one-prompt setup block (#57)

### Fixed
- Bound graceful shutdown (C2) + detector lock-during-send (C3) (#30)
- DB: RELAY_DB path override (footgun) + atomic message fan-out (#35)
- Security: bind 127.0.0.1 by default; refuse exposed bind without auth (#36)
- Security: dashboard XSS: DOMPurify sanitize + quote-safe escapers (#37)
- DB: rotated backups (VACUUM INTO) + drop DeleteProject no-op FK PRAGMA (#45)
- Release: SHA256SUMS + verify on install/update; fix Windows updater (#46)
- Security: allowlist writable settings keys (#47)
- Auth: trust loopback when RELAY_API_KEY set + refresh shipped skill (#58)

## [1.0.0] — 2026-06-13

First major release: full v2 platform rewrite on top of v0.5.0.

### Added
- v2 dashboard: professional design system, project-as-container nav, board / stats / notifications pages, hash router, message cinema
- Boot context Def.7: budget projection for boot memories/tasks/messages, cross-scope memories, rune-safe UTF-8 previews
- Linear connector: hand-written TaskConnector, webhook-less reconcile dispatch, settings-driven config + team picker, live mirror Kanban board
- Notifications: rules evaluator, actions, digest scheduler, REST API + UI panel
- Stats: agentic analytics aggregation API + SVG charts
- Custom agent avatars (photo/gif)

### Changed
- Dropped file locks: worktree isolation replaces advisory locks
- Token reduction: MCP discovery mode (−96% init tokens), markdown table outputs, slimmer schemas

### Fixed
- Preserve omitted identity fields on agent re-registration (#24)
- Installer uses the /api/health probe (#18) and scans deeper for Claude Code projects (#19)
- Lint + drop dead code (#28)
- Web a11y pass (focus rings, aria-labels, reduced-motion, AA contrast)

## 0.7.0 (never tagged) — 2026-04-19

Development milestone recorded between 0.5.0 and 1.0.0. No `v0.7.0` tag or GitHub release exists: this work first shipped inside 1.0.0, and 1.0.0 removed the spawn engine, workflows and vault before it was tagged (commit "refactor(slim): cut spawning + docs/vault"), so those items never reached a tagged release. Kept as written on 2026-04-19 (commit "docs: v0.7 CHANGELOG + webhook playground script").

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

Public beta: one binary, one SQLite file, 67 MCP tools, 100% local by default.

### Added
- `/health` REST endpoint: uptime, version, DB row counts for monitoring
- `move_task` MCP tool: move tasks between boards/goals with prefix resolution
- `batch_complete_tasks` + `batch_dispatch_tasks`: bulk operations in one call
- `list_tasks` filters: `status: "active"` excludes done/cancelled, `include_archived` toggle
- Auto-notifications: dispatching a task sends inbox messages to target agents
- Inline checklist: toggle checkboxes on kanban cards without opening the edit form
- Uber Go style compliance: 0 lint issues, golangci-lint CI on every PR

### Changed
- Default message TTL raised from 1h to 4h
- `get_session_context` compacted (~50-60% token savings) (#11)

### Fixed
- Archived tasks no longer appear in agent task queries
- Board dropdown in edit form showed empty names
- `assigned_to` field properly saved in edit form
- `apiError` uses proper JSON escaping

## [0.4.0] — 2026-03-10

### Added
- MCP token usage tracking: every MCP tool response is measured (bytes, tokens, project, agent, tool) and batch-inserted through a buffered channel; API endpoints `/api/token-usage`, per-project, per-agent, timeseries (hourly/daily buckets); auto-cleanup after 90 days
- Token usage in the UI: real-time counter in the colony header with a 24h/7d/30d toggle, agent detail sparkline, stats grid and top 8 tools, 24h consumption on galaxy planet hover

### Changed
- Kanban board, Trello-style: minimal cards with checklist progress bars and a full edit popup (title, assignee, description, checklist, priority, status, goal, board), Cancelled grouped with Done behind a toggle, boards filtered per project, in-place DOM patching instead of re-renders (#12)
- Galaxy view: 2–3 orbital rings with the largest planets outside, token badges on hover only
- Compact MCP responses: trimmed deliveries, memories, messages and tasks payloads; a typical `get_session_context` drops from ~45K to ~4.5K tokens

### Fixed
- Removed corrupted green mech sprites; idle animations desync per agent

## [0.3.4] — 2026-03-10

### Fixed
- Vault: `indexFile` errors are logged during a full reindex, so files that fail to index (missing documents in subdirectories) are visible

## [0.3.3] — 2026-03-10

### Fixed
- Project deletion cascades to all tables, including `conversation_members`, `conversation_reads`, `team_inbox` and `message_reads`, which fixes an FK constraint error
- UI: duplicate `ZOOM_STEPS` and scale-button declarations removed, which fixes `SyntaxError: Identifier 'ZOOM_STEPS' has already been declared`
- Installer: CRLF line endings fixed, and a relay hint is injected into CLAUDE.md
- Installer wrapped in a block for `curl | sh` pipe compatibility

## [0.3.2] — 2026-03-10

### Changed
- CI: release workflow made idempotent with `--clobber` uploads

### Fixed
- Nil pointer in `DB.Close()` when the database was opened read-only

## [0.3.0] — 2026-03-10

### Added
- Smart Messaging (#8): priority-based routing (P0/P1 trigger push notifications, lower priorities respect TTL), conversations (multi-agent group chats with invite/leave/archive), delivery tracking (`ack_delivery`, `mark_read`, receipts), SSE real-time activity stream, and message orbs animated between agents on the canvas
- Context budget pruning: `get_inbox({ apply_budget: true })` scores messages by `0.7×priority + 0.2×tagRelevance + 0.1×freshness` and greedily selects the best subset within the byte budget. P0 always bypasses it
- `create_project` MCP tool (#10): one-command onboarding with an 8-phase prompt that generates the project colony (CTO + adaptive worker profiles), auto or interactive mode, worker boot sequence, and first two sprints planned by the CTO
- `agent-relay update` CLI command: checks GitHub Releases, tries a source build first, falls back to the prebuilt binary; `--force` flag and automatic launchd/systemd restart
- Obsidian vault integration: `register_vault`, `search_vault`, `get_vault_doc`, `list_vault_docs` (FTS5 indexed)
- File locks: `claim_files`, `release_files`, `list_locks` (TTL-based, auto-broadcast)
- Task UX: markdown rendering in task notification cards, a Cancel button to reject tasks from the card, zoom controls (+/- buttons and keyboard shortcuts)
- Installer dependency audit: checks curl (required), go, cc, git, jq, python3 with clear warnings
- Test coverage for MCP handlers and the REST API (#6); reverse proxy docs, TLS troubleshooting and platform notes (#7)

### Changed
- SQLite tuned for concurrent agent workloads: WAL, busy timeout, connection pooling (#5)
- `list_tasks` truncates descriptions to 200 chars (~70% token savings)
- JSON keys auto-normalised to snake_case
- `.mcp.json` is backed up before merge and never overwritten
- `/relay` skill updated for create_project, budget pruning, vault and file locks

### Fixed
- Agents dispatching to the `"human"` profile trigger notification cards, kanban highlights and the My Tasks filter again
- Hook scripts guard for jq availability
- Repo URL corrected from `claude-agentic-relay` to `WRAI.TH` everywhere
- Setup UX: 13 issues from user testing (#9)

## [0.2.1] — 2026-03-08

### Changed
- UI: colony agent selection redesigned with Civilization-style macro to micro navigation (#2)

## [0.2.0] — 2026-03-08

### Added
- Opt-in authentication, CORS, rate limiting, body size limits (#1)

### Changed
- License switched from MIT to AGPL-3.0 (6c826f0)

## [0.1.1] — 2026-03-08

### Changed
- UI: vault docs grouped by project, reworked header layout (94a5944)
- UI: markdown rendered with marked.js (GFM) instead of a custom regex parser, so fenced code blocks, tables and H5 headings render correctly (3f0c00a)
- Open-source setup: gitignore, CONTRIBUTING with feature workflow and branching strategy, issue/PR templates (16174ef, b05e898)
- CI: installer tests run after a release completes (7f8964d)

## [0.1.0] — 2026-03-08

First public release: a single Go binary that runs a local MCP server for coordinating AI coding agents, backed by one SQLite file with FTS5 search, with a web UI on `localhost:8090`.

### Added
- MCP server at `http://localhost:8090/mcp` exposing 58 tools. Agents are persistent: a second `register_agent` with the same name picks up the same inbox, memories and tasks, and the `as` parameter lets one session act as several agents. (5dca82a, e5aaff3, 7486c1b)
- Messaging through `send_message`: direct, broadcast, team channel, questions to the human (`to: "user"`) and named group conversations with `reply_to` threading. Messages queue while an agent sleeps. When teams are configured, sending follows team boundaries and `reports_to` chains. (e862292, 77d4ba8)
- Shared memory with agent, project and global scopes, FTS5 search, confidence, layer, tags and version metadata. (f272d6c)
- Tasks with P0-P3 priorities dispatched by profile, a goal hierarchy (mission, project goals, agent goals) whose progress rolls up, kanban boards, teams and reusable profiles. `get_session_context` returns an agent's profile, pending tasks with their goals, unread messages, active conversations, relevant memories and vault docs in one call. (5a2791d, 94a3081)
- Vault: indexes a directory of Markdown files (for example an Obsidian vault) into FTS5 and re-indexes on change. Profiles can list vault paths that are loaded automatically into `get_session_context`. The relay's own documentation is embedded under the `_relay` project. (5b7f784, 10f2413)
- Claude Code activity ingestion through hook scripts, plus live SSE streams at `/api/activity/stream` and `/api/events/stream`. (e9d9c62, 073c4dd)
- Web UI: projects shown as pixel-art planets, agents as robots on each planet with hierarchy lines and message animations, plus kanban, vault browser and sidebars for messages, memories and tasks. (171dbd9, 89d01f3, c1cd64d)
- CLI subcommands (`status`, `agents`, `inbox`, `send`, `thread`, `stats`, `conversations`, `memories`) and an `ar` shortcut. (21b7ba4, 9e3b59e)
- Installers for macOS/Linux (`install.sh`) and Windows (`install.ps1`) that set up auto-start and the `/relay` skill, and a release workflow that builds binaries for macOS, Linux and Windows. (3c68969, 60b8563, f61d874, 7bf58bf)

[1.22.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.21.1...v1.22.0
[1.21.1]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.21.0...v1.21.1
[1.21.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.20.0...v1.21.0
[1.20.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.19.0...v1.20.0
[1.19.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.18.0...v1.19.0
[1.18.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.17.0...v1.18.0
[1.17.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.16.0...v1.17.0
[1.16.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.15.0...v1.16.0
[1.15.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.14.0...v1.15.0
[1.14.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.13.0...v1.14.0
[1.13.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.12.0...v1.13.0
[1.12.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.11.0...v1.12.0
[1.11.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.10.0...v1.11.0
[1.10.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.9.0...v1.10.0
[1.9.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.8.0...v1.9.0
[1.8.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.7.2...v1.8.0
[1.7.2]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.7.1...v1.7.2
[1.7.1]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.7.0...v1.7.1
[1.7.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.6.0...v1.7.0
[1.6.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.5.0...v1.6.0
[1.5.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.9...v1.5.0
[1.4.9]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.8...v1.4.9
[1.4.8]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.7...v1.4.8
[1.4.7]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.6...v1.4.7
[1.4.6]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.5...v1.4.6
[1.4.5]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.4...v1.4.5
[1.4.4]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.3...v1.4.4
[1.4.3]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.2...v1.4.3
[1.4.2]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.1...v1.4.2
[1.4.1]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.4.0...v1.4.1
[1.4.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.3.2...v1.4.0
[1.3.2]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.3.1...v1.3.2
[1.3.1]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.3.0...v1.3.1
[1.3.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.5.0...v1.0.0
[0.5.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.3.4...v0.4.0
[0.3.4]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.3.0...v0.3.2
[0.3.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/TsukumoHQ/WRAI.TH/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/TsukumoHQ/WRAI.TH/releases/tag/v0.1.0
