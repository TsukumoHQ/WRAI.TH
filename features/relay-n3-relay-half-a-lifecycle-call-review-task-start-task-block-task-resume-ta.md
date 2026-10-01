# [relay/N3 relay half] a lifecycle call (review_task/start_task/block_task/resume_task) made by the delegating service 'niwa' never transfers the lease or assigned_to: the doer keeps it

## Team : wraith-engine (tsukumo)
## Branch : wraith/03958111-niwa-keeps-doer (from main)
## Relay task : 03958111-69ff-4a7d-a955-c575778dc33a
## Trace : trace=ec908cd962f18d96ba2d10289475b489
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: doer claims + starts; ANY caller (delegating service, tokenless 'niwa', lead, exec) calls review_task -> status in-review, lease_holder and assigned_to unchanged (doer)
- [ ] 2. test: same for start_task, block_task, resume_task, complete_task called by a non-holder with authority
- [ ] 3. test: only claim_task / reclaim_task / update_task assigned_to move a lease, with their existing authority checks
- [ ] 4. test: a caller without authority for the transition is still refused as today
- [ ] 5. one-shot repair: non-terminal tasks with lease_holder/assigned_to = 'niwa' re-pointed to claimed_by (or last non-service holder); idempotent; ambiguous rows logged by id and untouched; count in receipt
- [ ] 6. Go suite green with -tags fts5

## 2. Root cause & decisions

# 03958111 — N3 relay half (re-scoped 16:40Z): a lifecycle move never moves lease_holder / assigned_to, whoever calls

ROOT_CAUSE (measured, red test reproduces the field drift): transitionTaskCode set worker = CALLER for every move into in-progress / in-review, except when the caller was an override actor (dispatcher, human, RELAY_OVERRIDE_ACTORS). On v1.24.0 the gate daemon is not a recognised delegating service (S5 warn data: 100% of its calls are tokenless), so e18260a's override branch never applied. start_task (in-review → in-progress after a reject) and resume_task are NOT fenced, so the daemon's unfenced call handed it the lease and assigned_to; from then the doer's own block / review is TASK_LEASE_FENCED ("lease is held by niwa") and only the founder can act. Every round re-drifts.

DECISION (cto-tsukumo ruling 16:40Z):
- worker for any in-progress / in-review move = the task's doer, whoever calls: prior lease holder, else assignee, else claimer; a delegating-service name is never taken as a doer. The caller becomes worker only when the task has no doer yet (start straight from pending = its claim). A delegating service with no doer to act for is refused (TASK_LEASE_FENCED) instead of being handed the task.
- Only claim_task / reclaim_task / update_task assigned_to move a lease (unchanged paths, pinned by TestLeaseMovesOnlyOnClaimReclaimReassign). Authority to perform a transition is unchanged (fence + override rules as before; a peer's review is still refused).
- block / complete keep releasing the lease (existing behaviour, also for the doer); assigned_to stays the doer.
- Boot repair (idempotent): NON-TERMINAL tasks whose assigned_to / lease_holder is a delegating service are re-pointed to claimed_by; only the field that IS the service moves (a real assignee is never overwritten); rows with no non-service claimer are left untouched and logged by id; done / cancelled rows are history and stay.
- Receipt: .niwa/receipts/n3-repair-prod-snapshot.txt (receipt=n3-repair-prod-snapshot.txt): live-DB snapshot, 6 non-terminal niwa-held → 1 (5 re-pointed; 74d06e55 in-review has no claimer, logged); second run re-points 0.

REJECTED ALTERNATIVES:
- Keying the fix on IsDelegatingService only (round 1): the daemon is not recognised as one on v1.24.0 (tokenless), so the drift continued; the ruling makes the rule caller-independent.
- Repair via audit "last non-service holder": lease grants are not audited (only releases); claimed_by is the reliable doer.

## review-wraith verdict: SHIP
Scope: internal/db/tasks.go, internal/db/task_lease.go, internal/db/db.go, internal/db/delegate_repair_test.go, internal/relay/delegated_lifecycle_test.go, .niwa/receipts/n3-repair-prod-snapshot.txt.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (via niwa slot run)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- the ambiguous-row log line repeats on each boot while such a row exists (74d06e55 needs a human re-point).
Notes: no schema change; transition CAS unchanged; repair is a per-row UPDATE at migrate on the single writer conn.
Tests: TestDelegatedLifecycle_DoerKeepsLeaseAndAssignment (niwa / dispatcher / human × review, start, block, resume, complete), TestDelegatedLifecycle_UnlistedCallerNeverTakesTheTask, TestLeaseMovesOnlyOnClaimReclaimReassign, TestDelegatedLifecycle_PeerReviewRefused, TestDelegatedLifecycle_SelfSetServiceGetsNoDelegation, TestDelegatedLifecycle_NoDoerRefusedNeverAssignedToService, TestRepairDelegateHeldTasks.

RED_EVIDENCE:
  cmd: niwa slot run -- go test -tags fts5 ./internal/db ./internal/relay -run 'TestDelegatedLifecycle|TestLeaseMovesOnly|TestRepairDelegateHeldTasks'
  test_sha: 1b2ef95
  output: |
    --- FAIL: TestRepairDelegateHeldTasks (0.11s)
        delegate_repair_test.go:32: repaired 4 task(s), want 3
        delegate_repair_test.go:49: done-row: assigned "dev-f" lease "", want "niwa" ""
    FAIL	agent-relay/internal/db	0.489s
    --- FAIL: TestDelegatedLifecycle_UnlistedCallerNeverTakesTheTask (0.12s)
        delegated_lifecycle_test.go:108: start_task by niwa: assigned_to = "niwa", want dev-a
        delegated_lifecycle_test.go:108: start_task by niwa: lease_holder = "niwa" (status in-progress), want dev-a
        delegated_lifecycle_test.go:109: block_task by dev-a: TASK_LEASE_FENCED: lease is held by "niwa"; "dev-a" is not the holder
    FAIL	agent-relay/internal/relay	1.440s

## 3. Files changed

```
.niwa/receipts/n3-repair-prod-snapshot.txt         |  14 ++
 ...-review-task-start-task-block-task-resume-ta.md |  97 ++++++++++
 internal/db/db.go                                  |   2 +
 internal/db/delegate_repair_test.go                |  55 ++++++
 internal/db/task_lease.go                          |  58 ++++++
 internal/db/tasks.go                               |  27 ++-
 internal/relay/delegated_lifecycle_test.go         | 211 +++++++++++++++++++++
 7 files changed, 456 insertions(+), 8 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-03958111-69ff-4a7d-a955-c575778dc33a
- 🟢 AC1: Test pins behavior; without the worker override the lease would move to niwa. Pass: 1383 tests, 11 packages, -tags fts5. — evidence: internal/db/tasks.go:866-884 — override branch sets worker to priorHolder/AssignedTo/ClaimedBy when niwa (IsDelegatingService) calls review_task/start_task; the new TASK_LEASE_FENCED guard fires only when no doer is on record. internal/db/tasks.go:978-994 (in-review UPDATE uses COALESCE so assigned_to stays dev-a) and tasks.go:1094-1106 (applyLeaseOnTransition sets lease_holder to worker=dev-a). — test: TestDelegatedLifecycle_DoerKeepsLeaseAndAssignment (delegated_lifecycle_test.go:73) — assertDoerKeeps checks assigned_to=dev-a and lease_holder=dev-a after the first by('review_task'). Mental revert of tasks.go:866-884: worker='niwa', applyLeaseOnTransition writes lease_holder='niwa' → test fails.
- 🟢 AC2: All four transitions exercised; test would fail without the worker override (start_task would refuse via the no-doer guard, block_task and review_task would write lease_holder='niwa'). — evidence: Same code path as AC1 — tasks.go:866-884 worker override applies to all in-progress/in-review transitions, and the blocked path clears lease without touching assigned_to (tasks.go:1014-1021). For blocked→in-progress resume via StartTask, lease released so worker=priorHolder='' → falls through to AssignedTo=dev-a. — test: TestDelegatedLifecycle_DoerKeepsLeaseAndAssignment (delegated_lifecycle_test.go:73-90) — by() loop drives start_task/block_task/resume_task by niwa; assertDoerKeeps verifies dev-a ownership survives each transition including the block releases lease (assertDoerKeeps:61-62 explicitly allows lease_holder='' when status=='blocked').
- 🟢 AC3: Behavior is unchanged by this diff for non-override actors; the fence logic predates the N3 fix and the test pins it end-to-end. — evidence: Peer is not the dispatcher (cto dispatched) and not in RELAY_OVERRIDE_ACTORS (default 'niwa'), so isOverrideActor returns false at tasks.go:859 and checkLeaseFence at tasks.go:141-166 returns TASK_LEASE_FENCED before any UPDATE. — test: TestDelegatedLifecycle_PeerReviewRefused (delegated_lifecycle_test.go:94-104) — peer review_task on dev-a's task: expects IsError; then verifies task.Status==in-progress, lease_holder==dev-a, assigned_to==dev-a (nothing moved).
- 🔴 AC4: Test passes and AC is technically met, but the test setup does not actually exercise the concern it names (svc would be refused even with is_service=false because it isn't a delegating service). A stronger test would set RELAY_OVERRIDE_ACTORS to include svc and assert svc-with-is_service is still refused — currently the implementation has no such guard to fail. Marked partial because the test does not pin a unique behavior; a future refactor that adds delegation via is_service would not be caught by this test. — evidence: internal/db/task_lease.go:130-135 — IsDelegatingService(name) checks human/user literals then defers to IsOverrideActorName(name), which reads RELAY_OVERRIDE_ACTORS only. There is NO code path that grants delegation based on the agents.is_service column; AC4 is satisfied by absence of such a code path. — test: TestDelegatedLifecycle_SelfSetServiceGetsNoDelegation (delegated_lifecycle_test.go:108-120) — registers svc with is_service=true, expects refusal. However, the test does NOT put svc in RELAY_OVERRIDE_ACTORS, so the refusal is the same fence path as the peer test, not a separate is_service guard.
- 🟢 AC5: All three properties verified: count returned and logged, ambiguous rows left untouched (service-claim stays niwa/''), idempotent. No receipt file in .niwa/receipts/, but the function's return value + log line is the receipt the AC asks for. — evidence: internal/db/task_lease.go:474-515 — repairDelegateHeldTasks selects tasks with non-empty assigned_to/lease_holder, filters to those held by a delegating service, and re-points via CASE WHEN to claimed_by (or leaves ambiguous rows untouched). The function returns the repair count and logs 'migrate: N3 repair re-pointed %d task(s)...' at line 509 and the ambiguous list at 512. Wired into migrate() at internal/db/db.go:1141. — test: TestRepairDelegateHeldTasks (delegate_repair_test.go:8-46) — five rows seeded (drift-both, drift-assigned, ambiguous, service-claim, clean); asserts repair count==2 after first run, idempotency (count==0 after second run), and final assigned_to/lease_holder values match expected for every row.
- 🟢 AC6: No regressions. verify-r1.log matches the local re-run. — evidence: MACHINE VERIFY in prompt: exit=0, 'go test -tags fts5 ./internal/...' passed in 11 packages. Re-ran locally: 'Go test: 1383 passed in 11 packages', exit 0. Targeted lifecycle suite: 4 passed. — test: Full -tags fts5 ./internal/... suite plus the four TestDelegatedLifecycle_* in delegated_lifecycle_test.go and TestRepairDelegateHeldTasks in delegate_repair_test.go.

### Round 2 — ❌ REJECTED by human:wraith-cto-2

## 5. Timeline

- round 1 → **reject** (review-03958111-69ff-4a7d-a955-c575778dc33a)
- round 2 → **reject** (human:wraith-cto-2)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `03958111-69ff-4a7d-a955-c575778dc33a`._
