# [relay/norms] obligations slice 2a: retire ACK quirks Q1, Q2, Q4 with explicit escalation chain

## Team : wraith-engine (tsukumo)
## Branch : wraith/obligations-s2a (from main)
## Relay task : 6b4369f0-86e3-45f2-ae14-00e9260b5c16
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. Q1: once an obligation is escalated, no later notify is sent for it (test Q1NoNotifyAfterEscalate); the equivalence test is updated to expect exactly this one difference.
- [ ] 2. Q2: the notify is a durable relay message type=fyi P2 (no wake) and the escalation a normal P1 message, both persisted even if the recipient is offline (test Q2DurableNotifyAndEscalate).
- [ ] 3. OQ2: the bearer is the assignee profile, or the profile pool for an unassigned ticket (any member discharges); the sanction target is the dispatcher (tests BearerAssigneeProfile, BearerPoolWhenUnassigned).
- [ ] 4. Q4/OQ4: max_depth=3; rung 2 resolves dispatcher.reports_to if active, else the project's is_executive agent, else founder, and one journal line names which rule fired; a human appears only as the last rung (tests Rung2Resolution, HumanOnlyAtEnd).
- [ ] 5. Each retired quirk has its own test; go vet clean; go test -tags fts5 -race ./internal/... green.

## 2. Root cause & decisions

# 6b4369f0 — ACK obligations slice 2a: Q1/Q2/Q4 retired, escalation chain, human last

See the commit body (PR body) for the chain, the NEW SETTINGS (ack_manager_age 90m, ack_human_age 4h) and the mass-fire guard.

ROOT_CAUSE: slice 1 reproduced the legacy ACK checker exactly, quirks included: a notify could arrive after an escalate (Q1); notices were push-only and lost when the dispatcher was offline (Q2); the dispatcher was the only target however long the task sat (Q4).

DECISION: ruling DEC-wraith-obligations-1 OQ1/OQ2/OQ4 plus wraith-cto's approval of the 3 picks (absolute-age rung settings, mass-fire guard, equivalence diff = exactly Q1).

## review-wraith verdict: SHIP
Scope: internal/db/{obligations.go,db.go,obligations_test.go}, internal/relay/{cleanup.go,obligations_equivalence_test.go,obligations_chain_test.go}.
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/... 0 FAIL (2 runs).

BLOCKERS: none.
- Single writer: every transition is one writer tx (obligation CAS + task guard + lower-rung close); rung-2/3 re-check the task under the writer lock (in-tx read, no no-op UPDATE on tasks).
- Non-destructive: notices are ordinary persisted messages with deliveries; P2 fyi no-wake, P1 do.
- Migration additive and idempotent: 2 INSERT OR IGNORE norms + one depth backfill UPDATE (SliceOneDepthBackfilled, SchemaAndOldDBMigrates).
- No mass fire at deploy (EscalatedBeforeChainSendsNothing covers both legacy marks and a slice-1 escalate row).

Tests: TestObligationsChain/{Q1NoNotifyAfterEscalate,Q2DurableNotifyAndEscalate,BearerAssigneeProfile,BearerPoolWhenUnassigned,Rung2Resolution/{ReportsToActive,ReportsToInactiveFallsToExecutive,NobodyFallsToFounder},HumanOnlyAtEnd,EscalatedBeforeChainSendsNothing}, TestObligations/SliceOneDepthBackfilled, TestACKEquivalence (Q1 diff), TestObligationsOneSanctionPerTick (updated: Q1 retired).

NITS (non-blocking):
- A task first seen past ack_human_age (relay down >4h) jumps straight to the highest due rung, like the legacy escalate-at-first-sight.
- 6 files vs the ticket's 5: internal/db/obligations_test.go only updates slice-1 counts (2 -> 4 seeded norms) and adds the backfill test; notifications.go was not needed.

## 3. Files changed

```
internal/db/db.go                              |  14 ++
 internal/db/obligations.go                     | 148 ++++++++++++----
 internal/db/obligations_test.go                |  46 +++--
 internal/relay/cleanup.go                      | 107 +++++++++---
 internal/relay/obligations_chain_test.go       | 232 +++++++++++++++++++++++++
 internal/relay/obligations_equivalence_test.go |  63 +++++--
 6 files changed, 517 insertions(+), 93 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `6b4369f0-86e3-45f2-ae14-00e9260b5c16`._
