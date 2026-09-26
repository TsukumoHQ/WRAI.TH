# [relay/tasks] holder or dispatcher can release an accepted-not-started task back to pending

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/task-release (from main)
## Relay task : 545a11d2-63f6-498b-b07d-44a2a7351fb7
## Trace : trace=a1eb7ad0d1ba0684bc17f91e9a655dfb
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 the holder releases an accepted task: status pending, claimed_by/claimed_at/assigned_to cleared, pending_since = now, one audit/progress row naming who released (test ReleaseByHolderReturnsToPending).
- [ ] 2. AC2 the dispatcher can release it too; any other agent is refused with nothing changed (tests ReleaseByDispatcher, ReleaseByOtherRefused).
- [ ] 3. AC3 release of an in-progress, done or pending task is refused with nothing changed (test ReleaseRefusedUnlessAccepted).
- [ ] 4. AC4 concurrent release + start on the same task: exactly one wins, no mixed state (test ReleaseStartRace, -race).
- [ ] 5. AC5 TestToolSchemaBudget green with constants unchanged, bytes delta in PR body; go vet clean; go test -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

# [relay/tasks] holder or dispatcher can release an accepted-not-started task back to pending

Task: 545a11d2-63f6-498b-b07d-44a2a7351fb7

ROOT_CAUSE: a doer that claimed a task it would not start had no way to hand it back. `update_task(assigned_to:"")` is a no-op (field case 188f1efa), `accepted → pending` is not in validTransitions, and `reclaim_task` only takes over from a dead holder. The task sat blocked for the 2h lease.

DECISION (ruling cto-tsukumo 5ba954c0; ed744dee: multiplex, no new tool):
- **Shape: `claim_task release=true`** (+72 B: 54714 → 54786, margin 2630 → 2558; toolSchemaBudgetBytes and toolSchemaHeadroomBytes unchanged). It costs fewer bytes than a `reclaim_task op` because it needs one boolean with no enum. `reclaim_task` would also need a new `op` enum and a rewrite of its dead-holder-only description, and its semantics (refuse a live holder) are the opposite of a voluntary handback. `claim_task` already multiplexes `next=true`.
- **`db.ReleaseTask`** (task_lease.go, next to ReclaimTask):
  - Refusals:
    - a status other than accepted gets TASK_STATE_CONFLICT;
    - an agent that is neither claimed_by nor dispatched_by gets `ErrTaskReleaseForbidden`, which the handler maps to FORBIDDEN;
    - a Linear-mirrored task gets `errLinearReadOnly`.
  - The status check comes first, so a started task always reads as a state conflict.
  - One UPDATE, CAS on `status='accepted' AND COALESCE(claimed_by,'')=<read>`. It clears assigned_to, accepted_at, claimed_by, claimed_at and the whole lease, and sets `pending_since = now` so the ACK clock restarts (c933b2f1). It also sets last_activity_at.
  - 0 rows gets TASK_STATE_CONFLICT, and nothing is written.
  - The audit row is a `lease_transferred` with reason `released`, and its actor is the releaser. It is best-effort after the CAS, same as ReclaimTask.
- **Handler:** emits `task.lease_transferred` (reason released) and pushes the status to the connector. It re-announces the task through `announceClaimable` with the releaser as sender, so the releaser gets no delivery and the other profile agents do. The announce is skipped when a blocked_by prerequisite still holds the task, same rule as dispatchCore.
- No schema change.

FILES: internal/db/task_lease.go, internal/relay/handlers_tasks.go, internal/relay/tools.go, internal/db/task_lease_test.go, internal/relay/handlers_tasks_test.go

TESTS:
- `TestReleaseTask` covers AC1-AC4:
  - `/ReleaseByHolderReturnsToPending`
  - `/ReleaseByDispatcher`
  - `/ReleaseByOtherRefused`
  - `/ReleaseRefusedUnlessAccepted`
  - `/ReleaseStartRace` (25 rounds under -race)
- `TestReleaseTaskOverMCP` covers the claim_task path end to end: FORBIDDEN for a bystander, one task.dispatched, 0 deliveries to the releaser and 1 to the other profile agent, TASK_STATE_CONFLICT after start.

## AC4 interpretation (ruled by wraith-cto 72b07696)
"Exactly one wins" applies to a release and a start that overlap on the accepted row. The CAS makes those exclusive: the loser gets TASK_STATE_CONFLICT, and the row is never left half-updated. A start whose read lands AFTER a committed release sees a pending task. That start is a sequential implicit claim (`start_task` from pending is allowed), not a lost race, and it is intentionally not refused. `/ReleaseStartRace` checks three things: never both lost; if the release won, nothing of the claim survives; if the start succeeded, the row is a coherent in-progress task held by the starter.

## review-wraith verdict: SHIP
Scope: internal/db/task_lease.go, internal/relay/handlers_tasks.go, internal/relay/tools.go, internal/db/task_lease_test.go, internal/relay/handlers_tasks_test.go
Gate: build -tags fts5 OK / vet -tags fts5 ./... OK / gofmt OK / test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK / TestToolSchemaBudget 54786 B, margin 2558

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- The re-announce content reads "Dispatched by: <releaser>" (the announceClaimable template). Correct as a sender, but the wording is dispatch-flavoured.
- After guard tool 1c56b1c3 (~500 B) the margin is ~2058 B, still over the 2048 headroom, so this does not strictly depend on reclaim #2 cca54020.

RED_EVIDENCE:
  cmd: go test -tags fts5 -count=1 -run 'TestReleaseTask' ./internal/db/ ./internal/relay/   (test files of b83bc3f=8f634c4 after rebase, non-test files at origin/main d2209d2)
  test_sha: de86d5e   (was 8f634c4 before rebase onto origin/main d78ba21)
  output: |
    # agent-relay/internal/db [agent-relay/internal/db.test]
    internal/db/task_lease_test.go:314:17: d.ReleaseTask undefined (type *DB has no field or method ReleaseTask)
    internal/db/task_lease_test.go:351:22: undefined: ErrTaskReleaseForbidden
    FAIL	agent-relay/internal/db [build failed]
    --- FAIL: TestReleaseTaskOverMCP (0.16s)
        handlers_tasks_test.go:331: bystander release: {"code":"TASK_STATE_CONFLICT",...,"message":"task ... already claimed (status \"accepted\") before claim by \"w2\" could apply"}, want a FORBIDDEN error
    FAIL
    FAIL	agent-relay/internal/relay	1.546s

## 3. Files changed

```
internal/db/task_lease.go             |  62 +++++++++++++++
 internal/db/task_lease_test.go        | 141 ++++++++++++++++++++++++++++++++++
 internal/relay/handlers_tasks.go      |  32 +++++++-
 internal/relay/handlers_tasks_test.go |  56 ++++++++++++++
 internal/relay/tools.go               |   1 +
 5 files changed, 291 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `545a11d2-63f6-498b-b07d-44a2a7351fb7`._
