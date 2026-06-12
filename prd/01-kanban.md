# PRD — Kanban panel

> Read [00-context.md](00-context.md) first. This panel renders the task board.

## Problem

Humans and agents need to see the work and its live execution state at a glance.
In Linear mode the planning lives in Linear, but the human shouldn't have to
leave the relay (and pay Linear round-trips) just to watch the board move. In
degraded mode there's no Linear, so the board is also where simple cards get
written.

## Goal

A fast, local, real-time task board that reflects Linear (read-replica) and
overlays the relay's execution state — so you watch cards flow
Todo → In Progress → In Review → Done as agents work, with zero Linear
round-trips per render.

## Users

- **Human (observer)** — glances at columns, reads a card, watches movement.
- **Agents** — read their assigned In Progress tasks from this same mirror.

## Scope

**In:**
- Columns by state: Todo / In Progress / In Review / Done (+ Backlog collapsed).
- Cards showing: `linear_key` (SYN-123), title, points, priority, labels,
  assignee-agent, sub-task progress (n/m), and **live execution overlay** —
  which agent claimed it, `claimed_at`, blocked indicator.
- Cycle context: filter to the **active cycle** by default; show cycle name/dates.
- Hierarchy: sub-issues rendered as nested/children (from `parent_task_id`).
- Real-time updates from mirror refresh (webhook) + agent overlay changes.
- Card detail (read): full description, comments, the temporal trail, PR link.

**Out:**
- **Linear mode: read-only.** No human authoring relay → Linear. Planning is
  done in Linear. Cards are not draggable to change Linear state (state is owned
  by CTO/agent/GitHub per the state machine).
- **Degraded mode only:** writing/editing native cards is allowed (creates
  native tasks, nested via `parent_task_id`). No points/cycles (native is dumb
  by design).
- No velocity / burndown here (that's the Stats panel).

## Functional requirements

1. **FR-1 Render from mirror.** The board reads the local `tasks` table
   (read-replica), never Linear directly. < 100ms render for a full cycle.
2. **FR-2 State columns.** Map `linear_state` → columns. Group by state, order
   within a column by priority then points then `dispatched_at`.
3. **FR-3 Execution overlay.** Each card shows the relay overlay: claimed-by
   agent avatar, blocked badge, "in review N min" using `in_review_at`.
4. **FR-4 Cycle filter.** Default = active cycle; allow "all" and per-cycle.
5. **FR-5 Hierarchy.** Parent cards show child roll-up (n/m done); expandable.
6. **FR-6 Real-time.** Subscribe to the existing relay event/SSE stream; apply
   mirror upserts + overlay changes without full reload.
7. **FR-7 Mode-aware writability.** Degraded mode → create/edit cards (native
   tasks). Linear mode → read-only; a card opens its `external_url` to edit in
   Linear.
8. **FR-8 Card detail.** Read-only panel: description, comments, temporal trail,
   PR/branch link (`linear_key`).

## Data

Reads: the mirror `tasks` table (both zones — see context schema). No new
storage. Degraded-mode writes go to native `tasks` rows (`source = "native"`).

## UX

- Contemplative-first: cards animate between columns as state changes (respect
  `prefers-reduced-motion`). Watching the board drain is part of the show.
- On-style with the existing dark canvas theme; reuse current kanban styling.
- A claimed card visibly "belongs" to its agent (color/avatar from the canvas).

## Dependencies

- The mirror (connector listener + reconcile poll) populating `tasks`.
- The relay event/SSE stream (exists).
- Kill goals → nested tasks (foundation; `goal_id` removed, `parent_task_id`
  is the hierarchy).

## Success metrics

- Board reflects a Linear change within the webhook latency (seconds), reconcile
  bounds staleness to the poll interval.
- Zero Linear API calls on board render.
- A human can answer "what's being worked right now and by whom" in one glance.

## Open questions

- Degraded-mode card creation: keep the current `.kb-form`, minus
  points/cycle/board-complexity?
- Do we show Done indefinitely or collapse after the cycle closes?
