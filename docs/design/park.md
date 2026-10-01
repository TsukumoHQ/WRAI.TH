# Parking: held work never climbs the ACK chain (task 7bcbdbd0)

Covers e7b96560 (demote to backlog), the relay half of niwa P0 c373a9ad, and
field W5 (a pending Linear mirror cannot be frozen; claim+block writes In
Progress to Linear). Line numbers are on origin/main 01b9d21.

## 1. Current path

Most of c373a9ad already shipped in d43d844e (8fb7ae5) and fea65594 (01b9d21).

| Piece | Where | State |
|---|---|---|
| `park_task(task_id, reason, until: founder \| [task:]<id>[@in-review])` | `internal/relay/handlers_park.go:24`, `internal/db/parking.go:78` | shipped, **pending tasks only** (`CodeTaskNotPending`, parking.go:99) |
| Park storage | `task_holds` row, `reason='parked'`, `park_until/park_reason/parked_by`; until a task = `blocked_by` edge tagged `{"parked":true}` | shipped |
| Unpark | `resume_task` on a parked pending task (`handlers_tasks.go:575`), `POST /api/tasks/{id}/unpark`; until-task unparks on that task done (or in review) via `settleHoldsTx` | shipped |
| ACK silence | `ackCandidate` (`obligations.go:100`) excludes open parked holds and profile `human`/`user`; `readyPredicate` (`org_edges.go:341`) never readies a founder park | shipped |
| Founder gate | one P1 alert per gate entry (`founder_gates.alerted_at` CAS), `GET /api/founder-gates` digest | shipped |
| `blocked_by` after dispatch | `task_edge` (`handlers_tasks.go:1699`) and `update_task blocked_by / blocked_by_remove` (`:1236`), dispatcher only | shipped |
| pending → backlog | allowed by `validTransitions` (`tasks.go:65`) but **no tool or API case exposes it** | gap (e7b96560) |
| Already-open ACK obligation on park | `CloseMootTaskObligations` (`obligations.go:362`) only closes when status ≠ pending; a parked task stays pending, so an open rung stays `active` (it stops escalating, it does not close) | gap |
| Backlog closes obligations as `fulfilled` | `obligations.go:400`: any non-pending, non-cancelled status = fulfilled | gap (wrong label) |
| Linear mirror parked | park writes nothing to Linear (good), but reconcile (`reconcile.go:111`) and webhook (`webhook.go:260`) still emit `task.dispatched` when the issue moves to a started state | gap (W5) |
| Linear claim | `claim_task` pushes `accepted` (`handlers_tasks.go:414, 520`), `resolveStateID` maps it to In Progress (`writer.go:120`) | gap (W5) |
| Linear status sync | `UpsertLinearMirror` rewrites `status` every tick (`linear.go:122`), so a status-based freeze (backlog, blocked) on a mirror is undone by the next poll | constraint |
| Who may park a mirror | `callerMayReassign`: dispatcher (`linear`, never a caller), executive, doer's lead chain (a pending mirror has no doer) ⇒ executives only | gap (W5) |

## 2. State model (the choice)

Two mechanisms, chosen by what the work waits on, never both on one task:

- **backlog (status)**: deliberately queued, nobody waits on it. Native tasks
  only. Invisible to `claim next`, no ACK. `demote_task` enters it,
  `promote_task` leaves it.
- **park (hold flag on a pending task)**: waits on the founder or on another
  task. Stays pending and claimable by id. Works for Linear mirrors because the
  hold lives in `task_holds`, which mirror sync never touches.
- **accepted / in-progress work is not parked.** The ACK chain never watches
  held work (`ackCandidate` requires pending); the only alert on held work is
  stale-held (`stale_held.go`), which already excludes `blocked`. A holder who
  must wait calls `block_task` (releases the lease, notifies). A park flag on
  held work would need its own lease rules for no new silence.

```
validTransitions (unchanged map, newly exposed edges marked *)
"backlog":   {"pending", "cancelled"}
"pending":   {"accepted", "in-progress", "done", "cancelled", "backlog"*}   // * via demote_task
park / unpark: no status change (pending → pending, task_holds row opened / released)
Linear mirror: demote_task refused (errLinearReadOnly, tasks.go:1862, plus hint: park_task)
```

## 3. Rules

1. **Authority.** `demote_task` and `park_task`: dispatcher, executive, or the
   doer's lead chain (`callerMayReassign`, unchanged). A pending Linear mirror
   adds the **profile_slug's lead** (the agent the issue routes to,
   `seed.ProfileSlug`) and its `reports_to` chain, so a lane lead can freeze its
   own queue without an executive.
2. **Record.** Every demote / park / unpark writes `audit_log` (`RecordAudit`,
   action `task.demoted` / `task.parked` / `task.unparked`, reason, until) in
   addition to today's dispatcher notice. Today park only notifies; no audit row.
3. **ACK norms on entry.** Demote and park close every active `ack.*`
   obligation of the task as `inactive` (evidence `{"parked":true}` or
   `{"task_status":"backlog"}`), not `fulfilled`. Change in
   `CloseMootTaskObligations`, quoted:

   ```sql
   -- candidate set gains: OR EXISTS (SELECT 1 FROM task_holds h WHERE h.task_id = t.id
   --                                AND h.released_at IS NULL AND h.reason = 'parked')
   -- moot gains:          OR t.status = 'backlog' OR <same parked EXISTS>
   ```
   `ackCandidate` already proves no rung opens while parked (`NOT EXISTS (… ph.reason = 'parked')`)
   or in backlog (`t.status = 'pending'`).
4. **ACK clock on exit.** `promote_task` and unpark restart the clock: both pass
   through the `pending_since = now` write (`tasks.go:936`) or the settle path
   that resets it. Test pins it.
5. **Linear, parked mirror.** Never written to In Progress:
   - `writer.go`: `PushStatus("accepted")` moves nothing (a claim is a
     reservation; `start_task` moves the issue). Park itself pushes nothing.
   - `linear_webhook.go` / reconcile: a parked mirror never emits
     `task.dispatched`; the issue may still be In Progress in Linear (a human
     moved it), the relay only refuses to launch an agent on it. Unpark
     re-dispatches if the issue is started.
6. **Batch.** `demote_task` takes `task_ids[]`; per-id result, one refusal does
   not abort the rest (pattern: `batch_dispatch_tasks`).

## 4. c373a9ad AC2 (`blocked_by` settable after dispatch)

Already satisfied: `task_edge add/remove` and `update_task blocked_by /
blocked_by_remove` (dispatcher only), tested by
`TestUpdateTaskBlockedByAfterDispatch`. No slice.

## 5. Slices

| # | Slice | Files | Test (AC) |
|---|---|---|---|
| P1 | `demote_task` (single + batch), native only, Linear mirror refused, audit | `handlers_tasks.go`, `tools.go`, `toolset.go`, `db/tasks.go`, `demote_test.go` | demote pending→backlog with reason audited; non-dispatcher refused; mirror refused with hint; batch reports each id |
| P2 | ACK close on demote/park as `inactive` | `db/obligations.go`, `obligations_park_test.go` | open `ack.notify` closes `inactive` on park and on demote; no rung opens on either; promote restarts clock from promotion |
| P3 | Park/unpark audit + mirror lane-lead authority | `handlers_park.go`, `handlers_tasks.go` (`callerMayReassign`), `park_audit_test.go` | park writes `task.parked` audit row; routed lead parks its pending mirror, a peer agent is refused |
| P4 | W5 Linear: claim moves nothing; parked mirror never dispatched | `connector/linear/writer.go`, `reconcile.go`, `webhook.go`, `linear_park_test.go` | claim+block on a team without Blocked state makes 0 `issueUpdate`; parked mirror whose issue goes started emits 0 `task.dispatched` on poll and webhook |
| P5 | `list_tasks status='backlog'` filter + board column, docs paragraph | `tools.go`, `web/static/v2/api.js`, `skill/relay.md`, `list_backlog_test.go` | `list_tasks status=backlog` returns only backlog tasks |

P4's first half is coded and green on `wraith/w3-w8-linear-mirror` (4a94617),
held until this ruling.

## 6. Open questions for the ruling

1. Mirror authority: add the routed lane lead (rule 1), or keep executives only?
2. Held work: confirm "block, not park" for accepted / in-progress (section 2).
3. A parked mirror whose Linear issue is moved to In Progress by a human:
   silent hold (rule 5), or also post one Linear comment "parked on the relay:
   <reason>"?
