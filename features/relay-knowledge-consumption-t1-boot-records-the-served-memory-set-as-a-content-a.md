# [relay/knowledge] consumption T1: boot records the served memory set as a content-addressed snapshot (zero writes when unchanged)

## Team : wraith-engine (tsukumo)
## Branch : wraith/consumption-t1 (from main)
## Relay task : 255947cd-8d22-4de5-b057-ba5c4e3b227a
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 write economy: TestConsumption/BootWritesOneTx (first boot -> 1 snapshot, 1 set, N edges, 1 head in one tx); /UnchangedBootWritesNothing (total_changes delta 0); /SetSharedAcrossAgents (same ids -> 1 set, N edges, 2 snapshots); /ConcurrentBootsOneHead.
- [ ] 2. AC2 fidelity: /ServedIdsArePostProjection (50 candidates, 10 served -> snapshot = ids in relevant_memories + decisions); /MinimalAfterFullRecordsEmpty; /MinimalAfterMinimalWritesNothing; /CaptureFailureDoesNotFailBoot (payload identical, error counted).
- [ ] 3. AC3 recall batch + retention + migration: /RecallBatchOneTxDropsHeadIds; /BufferBoundedDropsCounted; /PruneKeepsHeadsAndTaskBasis; /OldDBMigrates; existing TestSessionContext payload-ceiling tests stay green with basis.

## 2. Root cause & decisions

ROOT_CAUSE: the relay could not answer "which agents were served version N of key K": buildSessionContext computed the exact served set and threw it away, and memory_importance.go forbids per-recall writes, so no consumption record existed (design 54e529d8 §0, B-memory B1).
DECISION: record the post-projection served ids as a content-addressed context set; edges (memory id -> set) once per distinct set; a per-agent head so an unchanged boot writes nothing (prod replay: 82% of boots unchanged, <=170 writes/day vs ~1020 boots); minimal boots record the empty set once; capture best-effort (never fails the boot). Recalls buffered in memory (bounded, drops counted) and flushed in one tx per 1.5 s tick; prune keeps heads, recalls on heads and task_basis-referenced snapshots. Ruling f97023b7.
REJECTED: per-snapshot edges (2535 rows per 32.7 h vs 343 total); refreshing an unchanged head (liveness already in agents.last_seen); capture before projection (would record the 50 candidates, not what the agent saw).

ROUND 2: review round 1 was right (AC3 partial): recallBuffer.record had no production caller. Fixed in 7702f0a-based commit: get_memory, search_memory (ranked + plain) and recall_decisions record their returned ids (0 DB writes per call). TestRecallCaptureEndToEnd covers handler -> buffer -> flush -> DB. It also exposed a real gap, now fixed: WhoConsumed ignored head-less recall snapshots (an agent that recalled before ever booting was not a holder).

## review-wraith verdict: SHIP
Scope: internal/db/consumption.go (new), internal/db/db.go (migrate call after knowledge_log), internal/relay/project.go (unexported served id on MemorySummary/DecisionSummary, payload unchanged), internal/relay/handlers.go (boot capture + basis, recall buffer + flusher with Close drain), tests internal/db/consumption_test.go + internal/relay/handlers_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- 7 files vs the ticket's 5 (handlers_memory.go added for the round-1 wiring): the relay-level ACs (ServedIdsArePostProjection, CaptureFailureDoesNotFailBoot, BufferBoundedDropsCounted) need buildSessionContext/recallBuffer, so their tests live in internal/relay/handlers_test.go.
- session_context is no longer strictly read-only: at most one writer tx per boot, only when the served set changed (ruled).
- session_id is not stored yet (nullable); T2 can fill it when the stamp path has it.
- AC "BootWritesOneTx" is asserted structurally (one beginWriterTx in RecordSnapshot + row counts); there is no tx-count hook in the db package.

## 3. Files changed

```
...records-the-served-memory-set-as-a-content-a.md |  54 +++
 internal/db/consumption.go                         | 505 +++++++++++++++++++++
 internal/db/consumption_test.go                    | 244 ++++++++++
 internal/db/db.go                                  |   4 +
 internal/relay/handlers.go                         | 147 +++++-
 internal/relay/handlers_memory.go                  |  16 +
 internal/relay/handlers_test.go                    | 182 ++++++++
 internal/relay/project.go                          |  10 +-
 8 files changed, 1160 insertions(+), 2 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-255947cd-8d22-4de5-b057-ba5c4e3b227a
- 🟢 AC1: 4/4 named subtests green; one-tx and zero-when-unchanged behavior exercised by total_changes delta assertions — evidence: internal/db/consumption.go:179 RecordSnapshot writes set+edges+snapshot+head in one writer tx; :211 ON CONFLICT DO UPDATE WHERE set_hash<>excluded.set_hash serializes concurrent boots — test: TestConsumption/BootWritesOneTx, /UnchangedBootWritesNothing, /SetSharedAcrossAgents, /ConcurrentBootsOneHead (internal/db/consumption_test.go:31-93) all pass under -race
- 🟢 AC2: 4/4 named subtests green; payload integrity under capture failure verified by drop table + assert basis absent + payload len == 1; minimal boot writes nothing when head already empty — evidence: internal/relay/handlers.go:970-1002 served slice built from projectedMems[i].id and projectedDecs[i].id (post-projection), passed to captureBoot(..., SnapshotBoot, served) — test: TestConsumptionServedIdsArePostProjection (internal/relay/handlers_test.go:1998), TestConsumptionCaptureFailureDoesNotFailBoot (handlers_test.go:2017), TestConsumption/MinimalAfterFullRecordsEmpty and /MinimalAfterMinimalWritesNothing (consumption_test.go:95-119) all pass
- 🔴 AC3: [partial] DB-layer mechanics verified but the runtime recall pipeline (handler → buffer → flush → DB) ships unwired: flushConsumption goroutine runs every 1500ms but the buffer is always empty, so recall snapshots never get written in production. No integration test exercises the full pipeline. Fix: add h.recalls.record(project, agent, ids) at the end of HandleGetMemory/HandleSearchMemory/HandleRecallDecisions — evidence: internal/relay/handlers.go:1051 func (b *recallBuffer) record(project, agent string, ids []string) has ZERO production callers — grep for h.recalls.record / recalls.record returns only the drain site at line 1098. HandleGetMemory, HandleSearchMemory, HandleRecallDecisions (handlers_memory.go) call h.memReads.record (causal cache, different struct) but NOT h.recalls.record — test: TestConsumption/RecallBatchOneTxDropsHeadIds and /PruneKeepsHeadsAndTaskBasis and /OldDBMigrates pass via direct DB calls (consumption_test.go:121,149,236); TestRecallBufferBoundedDropsCounted passes (handlers_test.go:2079); TestSessionContext* pass with basis

## 5. Timeline

- round 1 → **reject** (review-255947cd-8d22-4de5-b057-ba5c4e3b227a)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `255947cd-8d22-4de5-b057-ba5c4e3b227a`._
