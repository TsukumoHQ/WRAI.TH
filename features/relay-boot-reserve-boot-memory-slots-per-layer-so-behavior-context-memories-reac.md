# [relay/boot] reserve boot memory slots per layer so behavior/context memories reach agents

## Team : wraith-engine (tsukumo)
## Branch : wraith/boot-layers (from main)
## Relay task : f4efab3a-8e6f-4952-a0a0-e4fadeb4c79d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 selection: a fixture with 80 live constraints, 20 behavior, 5 context memories in one project -> ListBootMemories returns at least the reserved quota of behavior and all 5 context, newest first per layer, total <= the limit; with 0 behavior/context the constraints fill the whole limit (quota refill). Test TestListBootMemoriesReservesLayers.
- [ ] 2. AC2 projection: projectMemories on that selection under sessionMemoryBudget keeps at least the newest behavior and newest context memory plus the constraint floor; memories_omitted counts the rest. Test TestProjectMemoriesLayerFloors.
- [ ] 3. AC3 unchanged contract: session_context keys and types unchanged, decision-layer rows still excluded, stale/expired rows still excluded. Test TestBootMemoryContractUnchanged (or extend an existing session_context test). PR body: per-layer counts of relevant_memories for 3 agents in tsukumo on a read-only COPY of a prod backup, before vs after.

## 2. Root cause & decisions

ROOT_CAUSE: ListBootMemories selected boot candidates with a single LIMIT 50 ordered constraints-first, so any project with >= 50 live constraints filled every slot with constraints; projectMemories then spent the whole 2600 B budget on the 8-constraint floor. Behavior and context memories could never reach session_context (measured 0/57 and 0/5, design af783f93 §1.3).
DECISION: reserve slots per layer in ListBootMemories (constraints 25, behavior 10, context 10; newest first per layer; unused quota refilled in the legacy order) and add a projection floor of the newest behavior and newest context memory next to sessionConstraintFloor. Ruling cto-tsukumo af6e3a5f: per-layer reservation, not importance rank.
REJECTED: importance-ranked selection (ruled out by af6e3a5f; changes which constraints surface); raising sessionMemoryBudget (reintroduces the WRAITH R1 payload bloat); excluding constraints beyond the floor from the candidate set (breaks memories_omitted semantics).

## review-wraith verdict: SHIP
Scope: internal/db/memories.go (ListBootMemories + selectBootMemories, BootQuota* consts), internal/relay/project.go (per-layer floors in projectMemories), tests in memories_boot_test.go + project_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK (978 passed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- memories.go ListBootMemories: candidate read is now <= 4 x limit rows (one window per layer group) instead of limit; bounded, read pool only.
- decision-layer rows still compete in the refill (as before) and are dropped later by buildSessionContext; unchanged behaviour.

Replay (read-only COPY of relay.20260925T225329Z backup, project tsukumo, buildSessionContext relevant_memories by layer):
- before 9f4c742: wraith-cto {constraints 8} omitted 42 | backend-lead {constraints 8} omitted 42 | gate-lead {constraints 8} omitted 42
- after:          wraith-cto {constraints 8, behavior 1} omitted 41 | backend-lead {constraints 8, behavior 1} omitted 41 | gate-lead {constraints 8, behavior 1, context 1} omitted 40

## 3. Files changed

```
internal/db/memories.go           |  99 +++++++++++++++++++++++++++++++----
 internal/db/memories_boot_test.go |  95 ++++++++++++++++++++++++++++++++-
 internal/relay/project.go         |  43 ++++++++++++---
 internal/relay/project_test.go    | 107 ++++++++++++++++++++++++++++++++++++++
 4 files changed, 326 insertions(+), 18 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `f4efab3a-8e6f-4952-a0a0-e4fadeb4c79d`._
