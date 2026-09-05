# [wraith/relay][split] Limbo sweep = DEC-wraith-limbo-sweep-rule-1: tier1 needs dispatcher inactive, tier2 auto-archive REMOVED, reason 'limbo-sweep', 1 audit row per block, shadow line 'limbo would-block'

## Team : wraith-engine (tsukumo)
## Branch : wraith/relay-limbo-sweep-align (from main)
## Relay task : c3d6dd3c-b27c-425e-ac64-c90d17f77246
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. TIER 1 blocks only when ALL of: assignee row status<>active & not service (existing SQL prefilter), task last_activity_at AND agent last_seen both older than 7d, AND dispatcher inactive (row missing or status not in active/sleeping); a task whose dispatcher is active or sleeping is skipped — covered by a new db test
- [ ] 2. TIER 2 auto-archive is gone: no archiveLimboTask, no archiveLimboAfter, no Archived result, no tier=2 log; a 40d-stale task with a gone dispatcher ends BLOCKED with archived_at NULL — covered by test
- [ ] 3. blocked_reason starts with 'limbo-sweep' and names assignee (with last_seen) and dispatcher; idempotence marker uses the same prefix; TestLimboSweepIdempotent still passes
- [ ] 4. apply mode writes exactly one audit_log row (action task.limbo_blocked, actor relay-sweeper, resource_id = task id, reason = blocked_reason) per successful block; dry-run writes zero tasks rows AND zero audit rows — asserted in TestLimboSweepDryRunWritesNothing
- [ ] 5. dry-run journals exactly one line per would-block in the format 'integrity: limbo would-block <task_id> <age>d <assignee> <dispatcher>' (age = whole days since last_activity_at); relay test asserts the format
- [ ] 6. go build ./..., go vet ./..., go test -tags fts5 ./... all green; no change under internal/relay/tools.go

## 2. Root cause & decisions

ROOT_CAUSE: The A1-ii limbo sweep as first shipped mis-modelled "limbo". A1-i proved the as-written rule (assignee GONE) was a no-op on all ~85 live limbo rows — a claimed task's assignee is inactive-by-construction, never absent — so flipping the apply flag alone would have been decorative. Worse, the tier-2 auto-archive stamped archived_at on stale tasks, which inverts the "tolerate, never cascade" doctrine (DEC-wraith-referential-integrity-phase3-1) and could silently drop a task a live lead still tracked.

DECISION (DEC-wraith-limbo-sweep-rule-1, cto-tsukumo 2026-09-05 19:12Z): a task is limbo only when ALL THREE hold — assignee inactive AND both clocks >7d stale AND the dispatcher itself inactive. Action is a single reversible BLOCK (status blocked, blocked_reason prefix "limbo-sweep" naming assignee + dispatcher), one audit row per successful CAS block (task.limbo_blocked, actor relay-sweeper). Never auto-archive, never delete; reversible by any lead via update_task/resume_task. Dry-run stays the default and journals one countable shadow line per would-block ("integrity: limbo would-block <id> <age>d <assignee> <dispatcher>") so the post-redeploy observation can be grepped and counted against the A1-i baseline before apply is flipped (cto-tsukumo only).

REJECTED ALTERNATIVES:
- Keep the tier-2 auto-archive (>30d): rejected — an immutable audit trail plus tolerate-don't-cascade means the sweep must never move a row to a terminal/hidden state on its own.
- Add a last_seen staleness threshold on the DISPATCHER: rejected — the ruling defines dispatcher-inactive as existence/status only (row missing OR status not in active/sleeping), no clock, so a long-quiet but still-active lead keeps the task out of limbo.
- Reassign the dead assignee's work to a live agent: rejected — DEC-wraith-update-task-reassign-1; a dead agent's work is quarantined (blocked), never silently handed off.

[LEGACY_OPPORTUNITY]: none new — this change removes the tier-2 archive path rather than adding surface.

## review-wraith verdict: SHIP
Scope: internal/db/limbo_sweep.go (+test), internal/relay/task_sweeper.go (+test) — limbo sweep alignment only.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (779 passed, 12 packages)

Thesis: no SSOT-silence risk (a maintenance sweep, apply-mode gated + dry-run default); single-writer intact (blockLimboTask + RecordAudit both via writerExec, read cursor drained/closed before any write); backward-compatible (zero schema, zero migration, additive log/audit only).

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- task_sweeper.go: apply pass now issues 2 writer txs per block (CAS block + one audit row). Fine here — limbo blocks are rare and this is the 2-minute maintenance ticker, not a request hot path; noted only for awareness.

## 3. Files changed

```
internal/db/limbo_sweep.go         | 153 ++++++++++++++++-----------------
 internal/db/limbo_sweep_test.go    | 171 +++++++++++++++++++++++++------------
 internal/relay/limbo_sweep_test.go |  13 +++
 internal/relay/task_sweeper.go     |  35 +++++---
 4 files changed, 229 insertions(+), 143 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c3d6dd3c-b27c-425e-ac64-c90d17f77246`._
