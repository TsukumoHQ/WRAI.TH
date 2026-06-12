# PRD — Stats panel

> Read [00-context.md](00-context.md) first. This panel shows agentic analytics.

## Problem

Linear already gives you **issue** analytics (cycle burndown, lead time per
issue). What it cannot see is how the **agent team** performs: an agent only
pushes `In Review` to Linear, so the fine execution timeline — claim → start →
blocked → PR — never reaches Linear. That timeline is the interesting part when
you manage an autonomous agent org, and right now it's invisible.

## Goal

Surface **agentic** analytics from the relay's auto-stamped temporal overlay:
agent cycle time, time-in-state, blocked time, throughput, and load — the
numbers Linear can't compute. The contemplative payoff: watch the curves fill as
agents drain the cycle overnight.

## Users

- **Human (observer)** — reads the team's health/throughput at a glance.
- Implicitly the CTO agent — could read these to rebalance load (future).

## What it shows (distinct from Linear)

| Metric | From | Why Linear can't |
|---|---|---|
| **Agent cycle time** — median claim→PR, claim→done | `claimed_at`, `in_review_at`, `done_at` | claim/PR sub-states never reach Linear |
| **Time-in-state** — Todo / In Progress / In Review distribution; bottleneck | temporal trail | finer than Linear's coarse states |
| **Blocked time** — total/avg per agent, what blocks | `blocked_at[]` | agents don't push "blocked" to Linear |
| **Throughput** — tasks & points completed per cycle / per agent | overlay + mirror points | per-agent attribution is relay-only |
| **Load** — who's active / idle / saturated now | live overlay (`claimed_by`) | Linear has no agent concept |
| **Burndown (mirror)** — cycle scope vs done | mirror | parallels Linear, rendered locally |

## Scope

**In:**
- The metrics above, scoped to the **active cycle** by default, with per-cycle
  and per-agent breakdowns.
- Charts: per-agent cycle-time (bar/box), time-in-state distribution (stacked),
  throughput over the cycle (line), blocked breakdown, current load.
- All computed from the local overlay + mirror — **no Linear round-trips**.
- Respect `prefers-reduced-motion` (data readable immediately; entrance anims
  optional).

**Out:**
- No velocity forecasting / planning (Linear owns planning).
- The relay **computes timing**, but does not author points/cycles.
- No per-issue Linear analytics duplication (link out to Linear for that).

## Functional requirements

1. **FR-1 Aggregation API** (`/api/stats?cycle=…`) computing the metrics from
   `tasks` overlay timestamps + mirror fields. Cheap; cache per-cycle short-TTL.
2. **FR-2 Auto-temporality dependency.** Requires the overlay to stamp
   `dispatched_at`, `claimed_at`, `started_at`, `blocked_at[]`, `in_review_at`,
   `done_at` on each transition (event-driven, zero manual input).
3. **FR-3 Per-agent + per-cycle dimensions.** Group/filter by agent and cycle.
4. **FR-4 Live load.** Current claimed/blocked/idle per agent from the live
   overlay; updates via the event stream.
5. **FR-5 Charts** — accessible (legends, tooltips, not color-only; text summary
   for screen readers per chart).
6. **FR-6 Empty/loading states** — meaningful "no data yet" when a cycle hasn't
   started; skeletons while computing.

## Data

Reads only: the overlay timestamps + mirror (`points`, `cycle_id`, `assignee`,
`linear_state`). No new storage beyond the temporal columns added for the
overlay (`claimed_at`, `blocked_at[]`, `in_review_at`; `dispatched_at`,
`started_at`, `done_at` largely exist).

## UX

- Dark, on-theme; charts legible at a glance (the human is an observer).
- The "money shot": a live cycle view where throughput + burndown advance in
  real time as agents work — designed to be left open on a second monitor.
- Tabular fallback for every chart (accessibility + exact numbers).

## Dependencies

- Auto-temporality stamping in the execution overlay (foundation for this panel).
- The mirror (`points`, `cycle`, `assignee`).
- The event stream for live load.
- A small chart approach consistent with the existing UI (no heavy lib unless
  justified).

## Success metrics

- Answers "how is the agent team doing this cycle" in one screen, with numbers
  Linear cannot produce.
- All metrics render from local data; zero Linear calls.
- Timestamps are 100% auto-filled (no manual input anywhere).

## Open questions

- Retention: how far back do per-cycle stats stay queryable (overlay rows persist
  vs archived)?
- Do we expose a CSV export for the data-heavy views?
