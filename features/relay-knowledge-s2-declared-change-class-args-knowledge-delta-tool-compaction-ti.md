# [relay/knowledge] S2: declared change_class args, knowledge_delta tool, compaction tick

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/knowledge-s2 (from main)
## Relay task : 1edb1377-f534-4d0b-88bb-523dc0aacbec
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 declared class: TestKnowledgeTools/SetMemoryChangeClassStoredAsDeclared; /InvalidChangeClassRejectedNothingWritten (INVALID_ARGUMENT, 0 rows). change_class on set_memory only (remember deferred).
- [ ] 2. AC2 delta tool: /DeltaReturnsNonEditorialSinceRev; /DeltaCompactedTrueBelowWatermark; /DeltaDoesNoDBWrite (total_changes unchanged). session_context knowledge_rev deferred to the follow-up (handlers.go, avoids collision with consumption T1).
- [ ] 3. AC3 compaction + budget: /TickCompactsAndIsIdempotent; TestToolSchemaBudget green with the bytes spent stated in the PR body (reported 632 B).

## 2. Root cause & decisions

ROOT_CAUSE: knowledge S1 records every knowledge change, but nothing exposed it. Writers could not declare a change class, agents had no read path for "what changed since rev R", and the log was never compacted, so the low-watermark/compacted signal was never produced.

DECISION:
- set_memory accepts an optional change_class (editorial|additive|narrowing|breaking), stored as declared_class. The relay's override floor still decides the effective class. An invalid value returns INVALID_ARGUMENT before any write.
- In-scope fix (cto-approved): HandleSetMemory computed causal=cache|arg|none but passed only BasedOn, so S1 derived "arg" for a cache-filled based_on. It now passes Causal.
- knowledge_delta(since_rev) is a thin read-only wrapper over db.KnowledgeDelta (RO pool; no write, proven with a data_version test).
- Compaction runs on the cleanup tick, throttled to one pass per hour (KnowledgeCompactInterval). A per-tick pass would open ~25 writer txs a minute for rows that only become eligible after the 30 d lag. Targets are ListProjectsFiltered(true) ∪ ProjectNames() ∪ '*'.

REJECTED:
- Compacting every tick: pure writer-lock churn.
- ListProjects only: it misses projects whose names come only from the projects table.

SCOPE (cto rulings): remember(change_class) and session_context knowledge_rev (+ AC /SessionContextCarriesKnowledgeRev) move to a follow-up (decisions.go + tools.go + handlers.go + test). The reason is that consumption T1 is editing buildSessionContext. S2 stays at 5 files. knowledge_min_compaction_lag is only read here; its settingSpec is declared by 00734b64.

SCHEMA BUDGET: +618 B (80 tools, 54596 B, margin 3366 -> 2748; headroom floor 2048). No toolset_test re-pin.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/relay/{tools.go,toolset.go,handlers_memory.go,cleanup.go,handlers_knowledge_test.go}
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 -race OK (1121 passed, 12 packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/relay/cleanup.go compactKnowledgeLogs: a project known only through memories (no agent/message/conversation row and no projects row) is not compacted. An exact DISTINCT project FROM knowledge_log reader needs a db file; it belongs in the follow-up.
- Single writer kept: compaction is one writer tx per project per hour; knowledge_delta reads only through d.ro(). The new MCP tool comes from the single registry (toolset.go) with resolveProject/resolveAgent.

REBASE r2 (onto beee189, after consumption T1 55e546c): handlers_memory.go append-only conflict (T1 memoryIDs helper vs S2 HandleKnowledgeDelta) — both kept. Gate green: db + relay -race; schema margin 2748.

## 3. Files changed

```
...lass-args-knowledge-delta-tool-compaction-ti.md |  68 ++++++++
 internal/relay/cleanup.go                          |  51 +++++-
 internal/relay/handlers_knowledge_test.go          | 176 +++++++++++++++++++++
 internal/relay/handlers_memory.go                  |  31 +++-
 internal/relay/tools.go                            |  11 ++
 internal/relay/toolset.go                          |   3 +-
 6 files changed, 335 insertions(+), 5 deletions(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-1edb1377-f534-4d0b-88bb-523dc0aacbec
- 🟢 AC1: Real handler-level exercise, including the 0-write invariant for the rejected case (verified end-to-end). — evidence: internal/relay/tools.go:306 adds change_class enum to set_memory only (remember tool at tools.go:310 unchanged, no change_class); internal/relay/handlers_memory.go:66-81 reads req.GetString change_class, routes via db.SetMemoryOpts.ChangeClass, catches db.ErrInvalidChangeClass to return validationError(CodeInvalidArgument, ...). db.SetMemoryWith (internal/db/memories.go:72-74) rejects invalid class BEFORE beginWriterTx so no rows written. — test: TestKnowledgeTools/SetMemoryChangeClassStoredAsDeclared + TestKnowledgeTools/InvalidChangeClassRejectedNothingWritten in internal/relay/handlers_knowledge_test.go (lines 42-90) — asserts declared_class+change_class stored when declared; asserts INVALID_ARGUMENT, 0 delta rows, 0 GetMemory rows on rejected write.
- 🟢 AC2: Behavioral: the no-write check uses a raw sqlite3 handle with PRAGMA data_version, not a mock or call-sequence assertion. — evidence: internal/relay/tools.go:325 adds knowledgeDeltaTool; internal/relay/handlers_memory.go:518-530 adds HandleKnowledgeDelta (read-only, calls db.KnowledgeDelta on d.ro()); internal/relay/toolset.go:214 wires into memory category (toolRegistry). db.KnowledgeDelta (internal/db/knowledge_log.go:326-363) filters editorial and uses the RO pool. session_context knowledge_rev not touched in this diff (deferred per feature doc). — test: TestKnowledgeTools/DeltaReturnsNonEditorialSinceRev (editorial touch row filtered, since_rev cursor correct) + DeltaCompactedTrueBelowWatermark (since_rev<watermark => compacted=true, since_rev>=watermark => compacted=false; state-complete after compaction) + DeltaDoesNoDBWrite (PRAGMA data_version unchanged across 3 calls). All in internal/relay/handlers_knowledge_test.go.
- 🟢 AC3: Tick integration exercises real cleanup tick body, not a stub. — evidence: internal/relay/cleanup.go:301-304 inserts compactKnowledgeLogs into runCleanupTick with KnowledgeCompactInterval=1h throttle; cleanup.go:351-391 compactKnowledgeLogs iterates targets = * + ListProjectsFiltered(true) + ProjectNames(), calls db.CompactKnowledgeLog per project. Idempotency: db.CompactKnowledgeLog (internal/db/knowledge_log.go:371-399) sets watermark=MAX removed rev and skips already-compacted rows on second pass. TestToolSchemaBudget passes: 80 tools, 54596 B, margin 2748 B (above 2048 headroom). PR body reports +618 B (AC text typo says 632 B). — test: TestKnowledgeTools/TickCompactsAndIsIdempotent in internal/relay/handlers_knowledge_test.go (lines 142-167) — runs compactKnowledgeLogs(d, now) (n=0 inside 30d lag), compactKnowledgeLogs(d, later) (n=3: 2 k1 + 1 global g1), compactKnowledgeLogs(d, later) (n=0), then runCleanupTick(d, st) and asserts st.lastKnowledgeCompact.IsZero()==false. TestToolSchemaBudget in internal/relay/toolsize_test.go:37-58.

## 5. Timeline

- round 1 → **approve** (review-1edb1377-f534-4d0b-88bb-523dc0aacbec)

**Approve-with-findings (follow-up):** validate green (1043 passed); TestKnowledgeTools 6/6 PASS; TestToolSchemaBudget 54596/57344 B (margin 2748); all 3 ACs verified by named behavioral tests

- **notice** `internal/relay/handlers_knowledge_test.go:1` — AC3 text states reported 632 B; PR body and feature doc both report +618 B (real TestToolSchemaBudget runs at 54596/57344 B, margin 2748 > 2048 headroom floor). Doc typo, test green.

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1edb1377-f534-4d0b-88bb-523dc0aacbec`._
