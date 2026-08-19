# changeset-per-run S1 — relay run-zone (schema + read)

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith/changeset-run-zone-s1 (from main)
## Relay task : 6641e3ad-5b5b-471f-b029-00c6dec8030f
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. run-zone fields on parent task (integration_branch, run_state) via additive migration
- [ ] 2. reuses parent_task_id/subtasks, NO new run_id entity
- [ ] 3. run_state transition set defined + enforced
- [ ] 4. relay stays inbound-only (no outbound gh/GitHub client)
- [ ] 5. single read returns run = parent + children + run-zone
- [ ] 6. review-wraith verdict in PR body; merges clean via gate

## 2. Root cause & decisions

# changeset-per-run S1 — relay run-zone (schema + read)

Task: 6641e3ad · Branch: wraith/changeset-run-zone-s1 → main · Design: trovex 7e8f1a8f · DEC reuse-parent

ROOT_CAUSE: A niwa multi-agent run today disperses into N independent tickets → N branches → N gates → N daemon-side merges to trunk. There is no atomic "run" unit and no single review surface, so the reviewer never sees cross-agent interaction and trunk gets N chances to break per run. This slice adds the relay-side foundation for grouping a run into one changeset.

## Decision
Represent a factory-run on the PARENT task, reusing `parent_task_id`/subtasks — NO new `run_id` entity. The parent IS the run ticket; its children are the agent slices. Add a run zone to the parent (mirrors the PR-link zone):
- `integration_branch` (TEXT) — the run's shared branch (niwa-set, off the real target).
- `run_state` (TEXT enum) — the run lifecycle.

`run_state` lifecycle (coordinated with cto-tsukumo, enforced in `SetTaskRun`):
```
""        → open                                  (first stamp must open)
open      → gating | blocked
gating    → merging | blocked
merging   → merged | blocked
blocked   → open | gating | amputated | merged
amputated → gating | merging | merged | blocked   (green subset proceeds)
merged    → ∅                                      (terminal, no resurrection)
```

Reads: `GetRun` = parent (with run zone) + subtask chain. Writes: `SetTaskRun` (COALESCE, transition-enforced). Both exposed as MCP tools `get_run` / `set_run` — MCP-only, like `link_pr`. The relay stays INBOUND-only (no GitHub client); niwa drives the branch + transitions.

Container guard (folds cto's greenlight AC): a task with `run_state` set is a run container — NOT claimable/startable as work (`ClaimTask`/`StartTask` → `RUN_CONTAINER_NOT_CLAIMABLE`). It groups slices; a worker claims a child slice, never the container. `ReclaimTask` already refuses a pending (unheld) task, so it needs no extra guard.

### Rejected alternatives
- **New `runs` table + `run_id` entity** — duplicates the subtask graph the relay already maintains (GetSubtasks/CheckSubtasksComplete/GetParentChain). Rejected: more schema, more surface, no gain over parent-reuse.
- **Stacked PRs instead of an integration branch** (S2 concern, noted): unordered sibling slices don't fit a linear stack; deferred to S2's branch strategy.
- **CAS-guarding the `run_state` UPDATE**: left as a COALESCE update (parity with `SetTaskPR`) because `run_state` is driven by the single-leader niwa daemon, not the multi-agent status machine. Flagged as a NIT for if concurrent run-state writers ever appear.

## Backward-compat / safety
Additive + idempotent migration (`ensureColumns`, nullable TEXT). Old DB + new binary: columns added at boot before any read. New DB + old binary: unknown columns ignored. taskColumns↔scanTask kept in exact lockstep (+2 both sides). Single-writer preserved (SetTaskRun on the writer conn; guard + GetRun on the RO pool). No new per-request hot-path write (set_run is niwa-driven, low-frequency).

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db (db.go col-map, tasks.go columns/scan/claim+start guards, new runs.go), internal/models/task.go, internal/relay (handlers_tasks.go, tools.go, toolset.go + count/budget test guards).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (477 passed, +7 new).

BLOCKERS: none.

NITS (non-blocking, for S2/engine):
- `SetTaskRun` run_state UPDATE is not CAS-guarded (parity with SetTaskPR; safe under niwa single-leader). Add `WHERE run_state IS <from>` only if concurrent run-state writers ever appear.
- A run-container parent keeps task-status `pending` while run_state advances. S2/engine should make the stale-scanner + notify rules skip run containers (`run_state != ''`) so a container isn't nagged or re-dispatched as an idle pending task.

## 3. Files changed

```
features/6641e3ad.md             |  82 ++++++++++++++++++++
 internal/db/db.go                |  10 +++
 internal/db/runs.go              | 138 +++++++++++++++++++++++++++++++++
 internal/db/runs_test.go         | 163 +++++++++++++++++++++++++++++++++++++++
 internal/db/tasks.go             |   8 ++
 internal/models/task.go          |  14 ++++
 internal/relay/handlers_tasks.go |  77 +++++++++++++++++-
 internal/relay/tools.go          |  21 +++++
 internal/relay/toolset.go        |   4 +-
 internal/relay/toolset_test.go   |   4 +-
 internal/relay/toolsize_test.go  |  15 ++--
 11 files changed, 525 insertions(+), 11 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `6641e3ad-5b5b-471f-b029-00c6dec8030f`._
