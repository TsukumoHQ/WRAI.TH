# PRD — Notifications panel

> Read [00-context.md](00-context.md) first. This panel configures signaling.

## Problem

Tasking and signaling were tangled together. Dispatch, manager alerts, agent
wake-ups, and digests are all "send something when something happens" — but they
were about to be hardcoded into the tasking logic. They belong in a **separate,
configurable subsystem** the human can edit and extend without touching code.

This is a clean re-incarnation of the removed trigger system: **decoupled from
tasking, decoupled from spawn, UI-driven, three columns — event / action /
target.**

## Goal

A configurable rules engine + screen where the human defines what gets sent,
on which event, to whom — and can add custom rules. Critically, the **agent
wake / external launch is itself a rule** (no hardcoded transport).

## Users

- **Human** — authors/edits/disables rules; wires their external launcher.
- The relay — evaluates rules on events and emits actions.

## Core model

```
RULE:  WHEN {event}  [IF {match}]  THEN {action}  → {target}  [opts]
```

**Events (semantic, low-token):**
`task.in_progress`, `task.claimed`, `task.blocked`, `task.in_review`,
`task.done`, `cycle.digest` (scheduled/coalesced), + **custom events** an agent
can emit by name.

**Actions:**
- `message` — relay inbox message (`ttl`, `priority`). Best-effort nudge.
- `webhook` — outbound POST (this is how the **external launcher** is wired:
  `WHEN task.in_progress THEN webhook → {launcher_url}` with `{agent, task_id,
  linear_key}`).
- `slack` / notify-channel — for the human.

**Targets:** an agent name, a role (CTO / Cxx / lead), the human, or a URL.

## Scope

**In:**
- Rules CRUD: list, add, edit, enable/disable, **test-fire**.
- The default rule set ships pre-seeded (see below) but is fully editable.
- Manager asymmetry baked into defaults: workers get per-task `task.in_progress`;
  managers get only exceptions (`task.blocked`, `task.in_review`) + `cycle.digest`.
- Payloads are tiny (`{agent, task_id, linear_key, 1-line}`) — never context.
- Coalescing for digests (one `cycle.digest`, not a stream).
- Outbound webhook signing (HMAC) so the launcher can verify the relay.

**Out:**
- No business logic / task mutation in rules (rules only *signal*; they never
  change task state — that stays in the state machine).
- No re-introduction of poll-triggers / signal-handlers / cycle-history from the
  old system.
- Not the inbound Linear webhook receiver (that's the connector, not a user rule)
  — though a `task.in_progress` event is what the connector emits into this engine.

## Default seeded rules

| When | If | Then | Target |
|---|---|---|---|
| `task.in_progress` | assignee is agent | webhook | external launcher (`{agent, task_id, linear_key}`) |
| `task.blocked` | — | message (P1) | the agent's manager (reports_to) |
| `task.in_review` | — | webhook | reviewer / human |
| `cycle.digest` | every 8h | slack/message | human (`Cycle N: x/y done, b blocked, r in review`) |

## Functional requirements

1. **FR-1 Rule storage + CRUD API** (`/api/notification-rules` …) and MCP-free
   (human-only via UI).
2. **FR-2 Evaluator.** On each relay event, match enabled rules, emit actions.
   Non-blocking, fire-and-forget.
3. **FR-3 Webhook action** with HMAC signature header + retry/backoff (bounded);
   log delivery outcome.
4. **FR-4 Message action** honoring `ttl` / `priority`; dispatch defaults to
   a short nudge (Linear SSOT means loss is harmless).
5. **FR-5 Digest scheduler** — coalesced periodic events (`cycle.digest`),
   computed from the mirror.
6. **FR-6 Custom events** — an agent emits `event:<name>`; rules can target it.
7. **FR-7 Test-fire** — dry-run a rule from the UI, show the would-be payload.
8. **FR-8 Manager asymmetry** — default routing so managers wake on exceptions
   only; in steady state a manager receives ~zero events.

## UX

- A 3-column rule list (event / action / target) with enable toggles + add row.
- An editor for a rule: event picker, optional match, action + target + opts.
- A delivery log (last N fires, success/fail) for debugging the launcher wiring.
- On-style with the dark theme; this replaces the removed "ops" slot conceptually.

## Dependencies

- The relay event bus (exists) to source events.
- `add_notify_channel` / outbound webhook plumbing (exists) for actions.
- The connector emitting `task.*` events into the bus.

## Success metrics

- The human can wire their external launcher entirely from the UI (no code).
- A manager in steady state receives near-zero notifications; exceptions arrive
  within seconds.
- Dispatch loss is invisible (agent still picks up work from the mirror).

## Open questions

- Rule scoping: per-project, or global with a project filter?
- Do we expose webhook secret rotation in the UI, or env-only?
