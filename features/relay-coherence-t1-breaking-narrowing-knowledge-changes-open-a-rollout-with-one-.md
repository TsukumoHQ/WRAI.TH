# [relay/coherence] T1: breaking/narrowing knowledge changes open a rollout with one reassess obligation per active consumer (advisory)

## Team : wraith-engine (tsukumo)
## Branch : wraith/coherence-t1 (from main)
## Relay task : ac528aa3-41e2-4a70-9a79-5afcd86450ad
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 who is disturbed: TestCoherence/BreakingOpensOnePerConsumer (A,B served K v1, C never; K->v2 breaking -> 1 rollout, 2 obligations, none for C); /AdditiveAndEditorialOpenNothing; /NewKeyOpensNothing; /TaskBeatsSession; /PendingCitingTaskObligesDispatcher; /CursorExactlyOnce (2 concurrent evaluateCoherence -> one rollout per rev).
- [ ] 2. AC2 closing: /MootWhenTaskDone; /MootWhenSupersededAgain (v3 supersedes the v2 rollout); /RebootAcks (acked_by reboot); /LeaseExpiryAcks; /PreparedRequiresServedNewVersion (discharge refused until get_memory(K) recorded); /CommitWhenAllClosed.
- [ ] 3. AC3 safety: /RoleRungNoHuman (breach -> reports_to rung; its breach -> committed_with_orphans, never user); /FanoutCapKeepsInFlight (over cap -> only leased-task obligations + an exception row, fanout_capped=1); /AdvisoryNeverRefusesClaim.

## 2. Root cause & decisions

ROOT_CAUSE: a breaking change to shared knowledge reached nobody: supersede was a silent overwrite, and nothing knew which active agents or tasks still ran on the old version, so stale work continued until a human noticed (design e731f3c9 §0, F-mas C1).
DECISION: sweeper cursor over knowledge_log (CAS) opens one rollout per breaking/narrowing/retraction row with a predecessor; one reassess obligation per active consumer (cited leased task -> assignee, cited pending task -> dispatcher, live context holder -> agent), on dedicated subject kinds so ACK sweeps never touch them; closure by maintenance condition, reboot ack, lease expiry; breach -> one supervisor rung, then orphan, never the human; fan-out cap 200 keeps in-flight work; prepared discharge re-checks the new version was served. Advisory only. Ruling 0019d6f5.
REJECTED: subject_kind 'task' for task consumers (CloseMootTaskObligations/StampLateDone act on every subject_kind='task' obligation regardless of norm); a task_premises table (ruled: citation at eval time); replaying history at deploy (cursor starts at the log head).

## review-wraith verdict: SHIP
Scope: internal/db/coherence.go (new), internal/db/db.go (migrate after consumption), internal/relay/cleanup.go (sweep + notices), internal/relay/handlers_obligations.go (mine + discharge routing), internal/db/coherence_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- Depends on budgets T2 (ced5ffc7) for LiveSupervisor / IsLiveAgent; branched on it, rebased onto main after that merge.
- A1 (claim-basis consumers) is not in this slice (ruled: lands with coherence T2 after consumption T2).
- Citation detection skips keys shorter than 6 characters (false-positive guard).
- AdvisoryNeverRefusesClaim is trivially true in T1 (no fence exists yet); it pins the contract for T2.
- Notices are P2 messages with action_required=do, one per new obligation (<= 200 per rollout by the cap).

## 3. Files changed

```
internal/db/coherence.go               | 720 +++++++++++++++++++++++++++++++++
 internal/db/coherence_test.go          | 316 +++++++++++++++
 internal/db/db.go                      |   4 +
 internal/relay/cleanup.go              |  29 ++
 internal/relay/handlers_obligations.go |  12 +
 5 files changed, 1081 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ac528aa3-41e2-4a70-9a79-5afcd86450ad`._
