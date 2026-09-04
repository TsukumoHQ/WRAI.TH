# [relay] update_task cannot reassign: profile_slug and assigned_to are ignored, a reassignment today = complete_task + dispatch_task (new id)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/update-task-reassign (from main)
## Relay task : a96968db-375c-4ae7-8d71-68a65c44b6c8
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 update_task with profile_slug and/or assigned_to changes the record; get_task reflects it; the existing claim (claimed_by) is cleared or transferred per an explicit rule stated in the decision
- [ ] 2. AC2 update_task with a non-updatable field returns an error naming the field instead of a silent no-op (named test)
- [ ] 3. AC3 the relay changelog/API doc lists the updatable fields of update_task
- [ ] 4. AC4 relay test suite green

## 2. Root cause & decisions

# a96968db — update_task reassignment (assigned_to / profile_slug)

ROOT_CAUSE: update_task never parsed or wrote profile_slug/assigned_to, so a reassignment silently returned 200 with an unchanged record. The only reassignment plumbing (DB.ReassignTask) was reachable solely via the HTTP POST /tasks/{id}/reassign route — the MCP tool surface every agent/orchestrator uses had no path to it. Orchestrators worked around it with complete_task + a fresh dispatch_task, duplicating the record and breaking trace_id continuity.

DECISION (implements DEC-wraith-update-task-reassign-1, CTO ruling a-e):
- update_task now accepts assigned_to and/or profile_slug and reassigns without changing status.
- (a) assigned_to on a CLAIMED task = atomic, CAS-guarded lease transfer in one UPDATE: claimed_by/claimed_at/lease_holder/lease_expires_at/lease_heartbeat_at repoint to the new agent, status unchanged, a LeaseTransfer{reason:"reassigned",by:caller} is stamped + audited, the old doer is notified.
- (b) profile_slug alone on a CLAIMED task = explicit refusal ("claimed by X; pass assigned_to…") — never silently re-profile held work.
- (c) PENDING task: both fields update freely, no lease minted (stays claimable); an explicit profile_slug wins over the recompute-from-assignee.
- (d) Only the dispatcher, an agent in the doer's reports_to lead chain, or an executive may reassign; a doer cannot reassign its own task.
- (e) Any unknown/non-updatable field is refused by name (whitelist), never a silent no-op — closing the original "ignored field returns 200" defect for the whole tool.

WHY CAS: a reassignment races a concurrent claim/complete/transfer; the UPDATE is guarded on the (lease_holder, status) read, returning TASK_STATE_CONFLICT on 0 rows rather than clobbering the winner (the double-claim TOCTOU the wraith gate guards).

REJECTED ALTERNATIVES:
- Reuse DB.ReassignTask as-is: it always mints a fresh lease (wrong for a pending task), uses reason "voluntary" not "reassigned", requires an agent (no profile-only path), and is not CAS-guarded. Added a dedicated ReassignTaskFields instead; left ReassignTask (HTTP path) untouched.
- Expose a separate reassign_task MCP tool: the ruling puts the behaviour on update_task (that is where orchestrators already reach for it and where the silent-ignore bug lives).

SCHEMA: none. LeaseTransfer gained a transient, non-persisted, non-scanned `By` field. Fully additive; older DBs and clients unaffected.

## review-wraith verdict: SHIP
Scope: internal/models/task.go (LeaseTransfer.By), internal/db/tasks.go (ReassignTaskFields, CAS), internal/relay/handlers_tasks.go (HandleUpdateTask unknown-field guard + reassignViaUpdate + callerMayReassign + taskHolder), internal/relay/tools.go (schema params), skill/tools-reference.md (AC3), + tests.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... = 693 passed.

BLOCKERS: none.
- Single-lane relay pipe, no schema/DB-push, no shared cross-lane file.
- Task transitions use a guarded conditional UPDATE + RowsAffected (no SELECT-then-UPDATE); no second writer; RO pool for reads.
- Additive + backward-compatible; discovery payload kept under the 16000-byte budget.

NITS (non-blocking):
- reassignViaUpdate reads the task once for guards, then ReassignTaskFields reads it again (two RO reads on a rare orchestrator path) — left for clarity over a micro-optimisation.

## 3. Files changed

```
internal/db/reassign_task_fields_test.go    | 111 ++++++++++++++
 internal/db/tasks.go                        | 101 +++++++++++++
 internal/models/task.go                     |  11 +-
 internal/relay/handlers_tasks.go            | 166 +++++++++++++++++++-
 internal/relay/tools.go                     |   4 +-
 internal/relay/update_task_reassign_test.go | 227 ++++++++++++++++++++++++++++
 skill/tools-reference.md                    |   2 +-
 7 files changed, 614 insertions(+), 8 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by human:cto-tsukumo

## 5. Timeline

- round 1 → **reject** (human:cto-tsukumo)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `a96968db-375c-4ae7-8d71-68a65c44b6c8`._
