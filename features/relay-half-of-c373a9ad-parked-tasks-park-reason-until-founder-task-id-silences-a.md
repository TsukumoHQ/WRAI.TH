# [relay half of c373a9ad] parked tasks: park(reason, until founder|task id) silences ack.notify/escalate/manager/human; unpark on dependency done or resume; blocked_by editable after dispatch; one alert per founder gate + open-gates digest

## Team : wraith-engine (tsukumo)
## Branch : wraith/d43d844e-park-task (from main)
## Relay task : d43d844e-af2f-4de2-b2fe-576ac8722665
## Trace : trace=fd642a6eb21553b393972926d637899d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. park_task(task_id, reason, until: founder | <task id>) + unpark/resume; parked tasks never trigger ack.notify, ack escalation, manager or human pages (test)
- [ ] 2. a task parked until <task id> unparks automatically when that task is done (test)
- [ ] 3. update_task accepts blocked_by (add/remove) from the dispatcher after dispatch (test)
- [ ] 4. exactly one alert when a task enters a founder gate (profile human or parked until founder); GET digest lists open founder gates with age (test)
- [ ] 5. go test -tags fts5 ./... green

## 2. Root cause & decisions

# d43d844e — parked tasks + founder gates (relay half of c373a9ad)

ROOT_CAUSE: the ACK ladder (ack.notify/escalate/manager/human) fires on every pending, ready, unclaimed task; the relay had no way to say "held on purpose". Tickets waiting on a busy assignee or on the founder (profile human, or a founder login) therefore climbed the ladder every tick-window and paged cto-tsukumo / niwa-cto repeatedly. blocked_by could only be set at dispatch (update_task refused it), so a dispatcher could not even express "after X" once the ticket existed.

DECISION:
- park_task(task_id, reason, until) reuses task_holds (reason 'parked', park_until/park_reason/parked_by columns). Held tasks were already skipped by the ladder (HeldTaskIDs); ackCandidate now also excludes parked holds and profile human/user tasks, so no obligation fires on them.
- until=<task id> = blocked_by edge tagged {"parked":true} + the hold: the existing settleHoldsTx/ReleaseReadyHolds releases it (and resets pending_since) when that task is done — no new release path.
- until=founder: readyPredicate treats an open founder park as not ready, so settle never auto-releases it and claim next skips it. Only resume_task (unpark) or the task leaving pending closes it.
- Founder gate = pending task with profile human/user or parked until founder. founder_gates table: one row per entry (partial unique index on open rows), alerted_at CAS = exactly one P1 alert to the founder per entry; GET /api/founder-gates = open gates + age. Sweep runs on the ACK tick and inline after park. Migration seeds already-pending human tasks as alerted (no burst at deploy).
- update_task blocked_by / blocked_by_remove, dispatcher only, via the same AddEdge/RemoveEdge as task_edge; applied before field edits so a refusal writes nothing.
- Park/resume authority = callerMayReassign (dispatcher, executive, doer's lead chain): a doer cannot park its own ticket to silence the ladder.

REJECTED ALTERNATIVES:
- Fold e7b96560 (demote pending -> backlog): different mechanism (status change, promote path, board view); parking keeps the task pending and claimable-by-id. Left e7b96560 as its own ticket.
- A separate unpark_task tool: resume_task already covers it (AC says "unpark/resume") and the tasks discovery payload was at 15996/16000 bytes.
- Raising the 16000-byte discovery cap: the test says split; obligation tools moved to an 'obligations' category (call_tool resolves by name, nothing breaks). Total schema cap 57344 -> 58368 per toolsize policy for new surface.

SCHEMA: additive only — task_holds gains park_until/park_reason/parked_by (ensureColumns); new table founder_gates + idx_founder_gates_open.

## review-wraith verdict: SHIP
Gate: go build -tags fts5 ./... OK / go vet OK / gofmt OK / go test -tags fts5 ./... exit 0.
- Transitions: alert via alerted_at CAS; gate open via partial unique index + INSERT OR IGNORE; hold release via existing CAS settle. No SELECT-then-UPDATE on a contended row.
- Single writer: all writes through writerExec/beginWriterTx; founder-gate close reads first and writes only when a gate leaves.
- Non-destructive inbox untouched; agentColumns/scanAgent untouched.
Tests: TestParkedTaskNeverTriggersAckRungs, TestParkUntilTaskUnparksWhenDone, TestUpdateTaskBlockedByAfterDispatch, TestFounderGateAlertsOnceAndDigest (internal/relay/park_test.go), TestFounderGateMigrationSeedsExistingAsAlerted (internal/db/parking_test.go).

NITS: obligations previously fired to the top rung stay closed after an unpark (pre-existing: fired obligations never re-open); unparked tasks restart the clock only for rungs not yet fired.

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/... ./internal/db/...   (origin/main 02443ef + the new test files only)
  test_sha: 749154e
  output: |
    # agent-relay/internal/db [agent-relay/internal/db.test]
    internal/db/parking_test.go:24:2: undefined: migrateParking
    internal/db/parking_test.go:25:2: undefined: migrateParking
    internal/db/parking_test.go:28:22: d.SyncFounderGates undefined (type *DB has no field or method SyncFounderGates)
    internal/db/parking_test.go:35:17: d.OpenFounderGates undefined (type *DB has no field or method OpenFounderGates)
    # agent-relay/internal/relay [agent-relay/internal/relay.test]
    internal/relay/park_test.go:29:2: undefined: evaluateFounderGates
    internal/relay/park_test.go:90:15: h.HandleParkTask undefined (type *Handlers has no field or method HandleParkTask)
    internal/relay/park_test.go:108:14: h.HandleParkTask undefined (type *Handlers has no field or method HandleParkTask)
    internal/relay/park_test.go:122:10: h.db.TaskParked undefined (type *db.DB has no field or method TaskParked)
    internal/relay/park_test.go:149:14: h.HandleParkTask undefined (type *Handlers has no field or method HandleParkTask)
    internal/relay/park_test.go:153:11: h.db.TaskParked undefined (type *db.DB has no field or method TaskParked)
    internal/relay/park_test.go:174:10: h.db.TaskParked undefined (type *db.DB has no field or method TaskParked)
    internal/relay/park_test.go:182:13: h.HandleParkTask undefined (type *Handlers has no field or method HandleParkTask)
    internal/relay/park_test.go:232:14: h.HandleParkTask undefined (type *Handlers has no field or method HandleParkTask)
    internal/relay/park_test.go:252:24: undefined: db.FounderGate
    internal/relay/park_test.go:252:24: too many errors
    FAIL	agent-relay/internal/relay [build failed]
    FAIL	agent-relay/internal/db [build failed]
    FAIL

## 3. Files changed

```
internal/db/db.go                |   2 +
 internal/db/obligations.go       |   8 +-
 internal/db/org_edges.go         |   6 +-
 internal/db/parking.go           | 280 ++++++++++++++++++++++++++++++++++++++
 internal/db/parking_test.go      |  42 ++++++
 internal/relay/api.go            |   2 +
 internal/relay/cleanup.go        |   1 +
 internal/relay/founder_gates.go  |  56 ++++++++
 internal/relay/handlers_park.go  | 136 +++++++++++++++++++
 internal/relay/handlers_tasks.go |  16 ++-
 internal/relay/park_test.go      | 286 +++++++++++++++++++++++++++++++++++++++
 internal/relay/tools.go          |  16 ++-
 internal/relay/toolset.go        |  11 +-
 internal/relay/toolset_test.go   |   8 +-
 internal/relay/toolsize_test.go  |   7 +-
 skill/relay.md                   |   2 +-
 16 files changed, 861 insertions(+), 18 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `d43d844e-af2f-4de2-b2fe-576ac8722665`._
