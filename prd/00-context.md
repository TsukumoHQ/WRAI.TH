# Agentic task layer — shared context

Background that the three panel PRDs (kanban, notifications, stats) build on.
Captured from the design session; this is the source of truth for the three.

## Vision

A 2-human-dev shop runs an **autonomous engineering org of agents**. Humans
observe; agents execute. The product is the **execution + observability layer**
for that org — not a planning tool.

- **CTO / Cxx agents** = management. They groom and document work.
- **Lead agents** = workers. They claim, code, open PRs.
- **Human** = observer. Glances at Todo / In Progress / Done + watches the
  contemplative canvas. The only routine human action is in the planner (Linear)
  or, in degraded mode, writing cards.

## Two modes

- **Degraded / native** — no external system. Tasks live in the relay DB
  (nested via `parent_task_id`; goals are removed). The kanban is writable
  (devs write cards). Self-contained, zero config.
- **Linear** — full fidelity. **Linear is the single source of truth (SSOT)**
  for work. The relay keeps a **read-replica mirror**, executes, and pushes a
  minimal execution signal back. Agents are treated as a dev team draining a
  Linear cycle.

## State machine (Linear mode)

```
CTO    Backlog → Todo → In Progress      (plan + release; In Progress = the GO)
agent  In Progress → In Review           (PR up)
GitHub In Review → Done                  (PR merge `Fixes SYN-123` → Linear auto-close)
```

Each transition has exactly one owner. The **Todo → In Progress** webhook is the
dispatch trigger (assignment can happen weeks ahead; the In Progress flip is
"launch now").

## SSOT + mirror (no bidirectional sync)

- Linear = SSOT. The relay **mirrors** it as a local read-replica so the board,
  viz, and agents read locally with **no Linear round-trips**.
- Sync is **one-way**: Linear → mirror, via **webhook** (realtime) + a
  **reconcile poll** of the active cycle (heals missed webhooks). On conflict,
  **Linear always wins** (reconcile overwrites the mirror).
- The relay writes to Linear in **exactly one place**: the agent's
  `→ In Review` transition + a comment. Nothing else flows up. Humans cannot
  author relay → Linear.

## Task schema (mirror)

Two zones:

**Linear zone (replicated, read-only — Linear writes, relay reads):**
`linear_issue_id`, `linear_key` (SYN-123), `external_url`, `title`,
`description`, `priority`, `parent_task_id` (from Linear sub-issues),
`cycle_id` / `cycle_name` / `cycle_dates`, `points`, `labels` (json),
`linear_state`, `assignee`.

**Relay overlay (owned, writable, auto-timestamped — the only thing that goes up):**
`claimed_by`, claim lock, `progress_notes`, `execution_status`, and the
**auto-stamped temporal trail**: `dispatched_at`, `claimed_at`, `started_at`,
`blocked_at[]` (start/end), `in_review_at`, `done_at` (from the Linear Done echo).

The relay **orders/filters** on cycle/points/priority but **computes nothing**
on them (no velocity — Linear owns that). Planning fields are passthrough.

## Dispatch (token-minimal)

- No persistent agents — work is assigned weeks ahead, so nothing runs while it
  waits (zero tokens during the wait).
- On In Progress: the relay pushes a **tiny event `{agent, task_id, linear_key}`**.
  A configurable notification rule (see notifications PRD) turns it into a
  webhook to an **external launcher** that spawns a fresh agent session.
- Knowledge lives **in the task** (CTO/chief-of-staff document it). The worker
  is self-sufficient from **one `get_task`** — no exploration, no inbox polling.
- The dispatch message is a best-effort nudge; if lost, no harm — the agent reads
  its In Progress tasks from the mirror (which reflects Linear SSOT) on spawn.

## Agent write surface

An agent can only: **leave comments** and **change status** (→ In Review). That's
it. Everything else is read.

## Connector size

1 listener (webhook In Progress → upsert mirror + fire dispatch rule) + 1 writer
(In Review + comment) + 1 reconcile poll (mirror freshness). Tiny.

## Four panels

| Panel | What | Fed by |
|---|---|---|
| Agents | the contemplative cinema | the JS canvas system |
| Kanban | the task board | the mirror read-replica |
| Notifications | configurable event→action rules | the decoupled notif subsystem |
| Stats | **agentic** analytics | the auto-stamped temporal overlay |
