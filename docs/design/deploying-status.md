# Deploying status: the relay half of the niwa post-merge pipeline (task 5330e6c7)

Slice S7 of the post-merge pipeline (39fc10ac, memory `niwa-postmerge-pipeline-ruling`,
design `~/Projects/niwa-cto/research/design-postmerge-pipeline.md` §3-4). Ruling: for a
pipeline repo, `complete_task` fires at `done` (deploy verified), not at merge. Between merge
and done the task needs a state that is neither "waiting on the gate" (in-review) nor "done".
Line numbers on origin/main 1c37c67. A prototype exists: aaaeec8 on `wraith/w3-w8-linear-mirror`.

## 1. Current path

| Piece | Where | Today |
|---|---|---|
| State machine | `internal/db/tasks.go:62` `validTransitions` | no `deploying`; in-review → {in-progress, done, blocked, cancelled} |
| Status column | `tasks.status TEXT NOT NULL` | **no CHECK constraint, no enum table**: a new value needs no table migration |
| Transition write | `transitionTaskCode` switch (`tasks.go:~1012`) | one CAS branch per target status; an unknown status has no branch (must add one) |
| Merge in a pipeline repo | niwa `finish_merge` → `complete_task` | task goes `done` at merge (wrong for pipeline repos) |
| PR sync | `webhook_pr_sync.go:16`, `reconcile_pr` | merged → done (would close a deploying task early) |
| Linear write | `writer.go:99` `resolveStateID` | no `deploying` case → "no matching state" comment |
| Linear read | `linear.go:149` `mapStatus` | a started state named "Deploying" reads `in-progress` → dispatches an agent |
| Enums shown to clients | `tools.go:790` list_tasks `status` enum; web `kanban.js`, `v2/api.js` | no `deploying` |

## 2. State machine (validTransitions form)

```
"in-review": {"in-progress", "done", "blocked", "cancelled", "deploying"*}   // * merge in a pipeline repo
"deploying": {"done", "blocked", "cancelled", "in-review"}                    // new
"blocked":   {"in-progress", "in-review", "done", "cancelled", "deploying"*}  // * human-retried deploy
```

| Pipeline outcome (design §3-4) | Relay move | Who |
|---|---|---|
| merged (pipeline) / deploy_pending / verify_pending | in-review → **deploying** (once; later stages are comments, deduped per stage) | niwa |
| verified (done) | deploying → **done** (`complete_task`, result carries `deploy` + `verify`) | niwa |
| deploy_failed / verify_failed | deploying → **blocked** (`block_task`, reason = first failure line; rollback proposal in the comment) | niwa |
| on_verify_failed = hold_target | same blocked move; the TARGET hold is niwa's (red-main hold), not a relay state | niwa |
| rollback (`niwa qa rollback`, human) re-verify green | task **stays blocked** (the change is still bad, design §5); the fix is a revert task | human |
| deploy retried after a fix to the pipeline | blocked → **deploying** | human / niwa |
| merge reverted before verify | deploying → **in-review** (back to the gate) or cancelled | lead / niwa |

## 3. Behaviour while deploying

- **Lease**: kept by the doer (N3 ruling 03958111: lifecycle moves never move the lease).
  `SweepExpiredLeases` only sweeps accepted / in-progress / in-review, so an expired lease
  on a deploying task is never requeued to pending. deploying → done / blocked is fenced
  like done (holder or override actor) and releases the lease as today.
- **WIP**: not counted (`wip_limit.go:56` counts accepted / in-progress only): the doer is
  free to claim its next ticket while its merge deploys.
- **ACK obligations**: none open (`ackCandidate` requires pending); stale-held does not fire
  (accepted / in-progress only); P6 lane-busy treats a doer holding only deploying work as
  idle, correctly.
- **blocked_by edges**: an `@in-review` dependent stays ready when its prerequisite moves on
  to deploying (`readyPredicate`, `TaskReadiness`, `ackCandidate`: `rp.status IN
  ('in-review', 'deploying')`); a default (`@done`) dependent waits for done — deploy
  verified, which is the point.
- **pr_reconcile / PR webhook**: a `merged` observation on a deploying task is a no-op (the
  merge is already accounted for; never resurrects, never closes early). `closed-unmerged`
  cannot happen after a merge.

## 4. Linear mapping

| Direction | Rule |
|---|---|
| relay → Linear (`writer.go` resolveStateID) | `deploying` → the team state whose NAME contains "deploy" (any type). None (team without a Deploying state, H1 not done yet): **no state move, issue stays In Review**, the move is recorded as a comment `relay: deploying (no Deploying state in Linear)`. |
| Linear → relay (`linear.go` mapStatus) | a started-type state named Deploying → `deploying`. Never a launch: excluded from reconcile and webhook dispatch (like In Review); `isTerminalOrActive` includes deploying so the poll never re-dispatches. |
| Accepted / In Progress resolution | the started-state pick for in-progress skips review-, blocked- and deploy-named states. |

Linear's own "PR merged → Done" automation must be off in the workspace (human step H1),
else Linear closes the issue at merge regardless of the relay.

## 5. Migration and old clients

- **Schema**: `tasks` unchanged. `tasks.status` is free TEXT; no CHECK to widen. One additive
  side table `task_deploys(task_id, project, merge_sha, entered_at)` (D1, ruling Q1) records the
  merge sha that entered deploying; blocked → deploying is refused unless the task was blocked
  out of deploying and carries that sha. Only code enums change:
  list_tasks `status` enum gains `deploying`; `status='active'` already includes it
  (`NOT IN ('done','cancelled')`).
- **Old clients** read `status` as an opaque string: a deploying task shows its raw status;
  boards that do not know it fall back to their default column (v1 kanban `columnFor` →
  Todo; v2 → its default) until slice D5 adds the column.
- **Binary rollback** to a relay without D1: `validTransitions["deploying"]` is nil, so any
  non-operator move out of deploying is refused (`invalid transition`); the operator can
  force-move (`POST /api/tasks/{id}/transition` with force). No data loss.

## 6. Slices (each ≤ 5 files, one test per AC)

| # | Slice | Files | Test (AC) |
|---|---|---|---|
| D1 | State machine + CAS branch + lease kept + `@in-review` edge readiness + REST transition case | `db/tasks.go`, `db/org_edges.go`, `db/obligations.go`, `relay/api.go`, `db/deploying_test.go` | in-review → deploying → done (and → blocked, → in-review); in-progress → deploying refused; lease stays with the doer and is never swept; `@in-review` dependent stays ready |
| D2 | MCP `deploy_task` (fenced like review) + list_tasks enum | `relay/tools.go`, `relay/toolset.go`, `relay/handlers_tasks.go`, `relay/deploy_task_test.go` | tool moves in-review → deploying; non-holder non-override refused; list_tasks `status=deploying` filters |
| D3 | PR sync honours deploying | `relay/webhook_pr_sync.go`, `relay/handlers_tasks.go` (reconcile_pr), `relay/pr_deploying_test.go` | merged webhook / reconcile_pr on a deploying task: no status change |
| D4 | Linear both ways | `connector/linear/writer.go`, `linear.go`, `reconcile.go`, `webhook.go`, `linear_deploying_test.go` | deploying → Deploying state; no state → no move + one comment; Deploying issue maps to deploying and dispatches 0 |
| D5 | Board column + docs | `web/static/js/kanban.js`, `web/static/v2/api.js`, `skill/relay.md` | board shows a Deploying column (UI check receipt) |

D1 and D4 reuse aaaeec8 (prototype, suite green at the time) minus its lease release.

## 7. Open questions for the ruling

1. `blocked → deploying` for a retried deploy: allow (as above), or force a fresh pass
   through in-review?
2. Team without a Deploying state: stay In Review + comment (recommended) or move to In
   Progress?
3. Lease on deploying: kept by the doer (recommended, N3-consistent) or released like done?
