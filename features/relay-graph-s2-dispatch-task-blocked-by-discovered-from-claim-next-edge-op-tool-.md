# [relay/graph] S2: dispatch_task blocked_by/discovered_from, claim-next, edge op tool, held announce + sweeper

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/graph-s2 (from main)
## Relay task : c15136bd-8067-461f-abbb-a3aaebc57b63
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 held dispatch: TestTaskGraph/DispatchBlockedByHoldsDelivery (B blocked_by A: 0 deliveries and no task.dispatched for B; complete_task(A): exactly one delivery per profile agent and one task.dispatched for B); /SweeperAnnouncesMissedRelease (a status write bypassing the handler, as Linear sync does, is announced once on the next tick).
- [ ] 2. AC2 claim: /ClaimNextOverMCP (claim_task next=true sort=unblock_impact returns the ready task with highest unblock_impact; {task:null, ready_count:0} when none); /ClaimNotReadyWarnsWithBlockers (claim_task on a held task succeeds, readiness.ready=false, blockers listed by id and title).
- [ ] 3. AC3 edges + budget: /EdgeToolRegistryErrors (task_edge unknown type -> INVALID_ARGUMENT; cycle -> EDGE_CYCLE with the path); TestToolSchemaBudget green, bytes stated.

## 2. Root cause & decisions

ROOT_CAUSE: graph S1 (ef72aba) built org_edges, the readiness predicate and holds in the DB, but nothing on MCP used them. dispatch still announced every held task, no agent could pull the next ready task, and a release was never announced.

DECISION:
- dispatch_task gets blocked_by (string array, "id" or "id@in-review", short ids resolved) and discovered_from (profile may be omitted and is then inherited). dispatchCore skips announceClaimable when db.TaskHeld: no delivery and no task.dispatched, same shape as the backlog skip.
- Exactly-once release announce (ruling b3a6ab43 + cto HARD REQ): task_holds gains announced_at (ensureColumns; holds that predate the column are stamped at migration because S1 handlers already announced them). MarkHoldAnnounced is `UPDATE ... WHERE released_at IS NOT NULL AND announced_at IS NULL` and only rows affected == 1 announces. There is no read-then-emit. The hold re-open resets it. left_pending closes set it.
- Inline path: complete, batch_complete, review, cancel and task_edge remove announce task.Released after commit (announceClaimable + task.released event).
- Sweeper: releaseHeldTasks (cleanup.go) runs ReleaseReadyHolds, announces what it released, then re-announces UnannouncedReleases older than a 60 s grace (crash window, Linear-sync status writes). It is wired as 1 line in the task maintenance tick (task_sweeper.go).
- claim_task: task_id optional with next=true + sort (priority|oldest|unblock_impact), pulling from the caller's registered profile. It returns {task:null, ready_count} when nothing is claimable. A claim of a task with unmet prerequisites succeeds with readiness {ready:false, blockers:[{id,title,status,until}]}, read before the claim.
- One task_edge(op=add|remove, task_id, type, target_id, until?) tool. type is a free string, so an unregistered type reaches the registry and gets INVALID_ARGUMENT. A cycle returns EDGE_CYCLE with the path. list_tasks gains ready (the same predicate).
- Found in self-review, fixed in scope: a held task is pending and unclaimed, so it entered the ACK ladder aged from dispatch and escalated towards manager/human while deliberately held. Now evaluateObligations skips open holds (HeldTaskIDs, one RO read per tick when obligations exist), and a release sets pending_since=now in the release tx. That restarts the ACK clock, as re-entering pending already does (c933b2f1).

SCOPE BUMP (cto-approved 6a9e5c4f / e1e63144): +internal/relay/task_sweeper.go (1 line: the sweeper needs *Handlers for announceClaimable and the EventBus; the cleanup.go tick has neither). +internal/relay/toolset.go (1 line: task_edge registration). +internal/relay/toolset_test.go (tasks tool count 25 -> 26).

SCHEMA BYTES: main fd8f730 = 81 tools / 53347 B. Branch = 82 tools / 54483 B, which is +1136 B, margin 2861 (headroom 2048). task_edge alone is 647 B (the fixed per-tool overhead is ~465 B). dispatch_task +~210, claim_task +~150, list_tasks +~45. The descriptions are already trimmed.

REJECTED: a goroutine started from handlers (cto: reject). A separate claim_next_task / add_task_edge / remove_task_edge tool (multiplex rule ed744dee). Mark-after-announce: it gives at-least-once, and the ruling wants exactly-once; the residual window is a crash between the CAS and announceClaimable (in-process, microseconds).

TESTS: TestTaskGraph/{DispatchBlockedByHoldsDelivery, SweeperAnnouncesMissedRelease, InlineAndSweeperRaceAnnounceOnce (20 rounds, concurrent inline + sweeper => exactly 1 task.dispatched), ClaimNextOverMCP, ClaimNotReadyWarnsWithBlockers, EdgeToolRegistryErrors, HeldTaskSkipsACKLadder, DiscoveredFromInheritsProfile}. Mutation-checked: CAS forced true => race test fails; held skip off => ACK test fails; pending_since reset off => ACK test fails.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/org_edges.go, internal/relay/{cleanup.go,handlers_tasks.go,handlers_tasks_test.go,task_sweeper.go,tools.go,toolset.go,toolset_test.go}
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK / go test -tags fts5 ./... OK / TestToolSchemaBudget OK (margin 2861)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking, follow-up candidates):
- handlers_tasks.go promote_task: a backlog task dispatched with blocked_by gets no hold (S1 holds only pending tasks), so promote announces it even when it is not ready.
- obligations_mine still lists a held task in the profile pool (the obligation row is instantiated, and only the rungs are skipped).
- Design §3.3 "one P2 message to the dispatcher when a prerequisite is cancelled" is not in this ticket. The flag is set (S1), and no message is sent yet.
- list_tasks ready=true ignores status / priority / assigned_to / include_archived (ready implies pending and not archived).

## 3. Files changed

```
internal/db/org_edges.go              |  89 +++++++++-
 internal/relay/cleanup.go             |  50 +++++-
 internal/relay/handlers_tasks.go      | 190 +++++++++++++++++++++-
 internal/relay/handlers_tasks_test.go | 295 ++++++++++++++++++++++++++++++++++
 internal/relay/task_sweeper.go        |   1 +
 internal/relay/tools.go               |  23 ++-
 internal/relay/toolset.go             |   1 +
 internal/relay/toolset_test.go        |   7 +-
 8 files changed, 640 insertions(+), 16 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c15136bd-8067-461f-abbb-a3aaebc57b63`._
