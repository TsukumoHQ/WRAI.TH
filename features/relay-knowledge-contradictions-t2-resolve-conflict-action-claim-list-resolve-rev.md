# [relay/knowledge] contradictions T2: resolve_conflict(action=claim|list|resolve|revert) with reversible invalidation

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/contradictions-t2 (from main)
## Relay task : 0090fabd-e5eb-4129-95f1-54f73565db75
## Trace : trace=8f4c6d334fdfc7f460103956d50520b9
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 resolve_conflict(action=resolve, resolution=merge) by the lease holder archives the loser with invalidated_by set and writes a knowledge_log retraction row that triggers a coherence rollout; a non-holder is refused with nothing written (tests /MergeArchivesWithInvalidatedByAndLogsRetraction, /OnlyLeaseHolderResolves).
- [ ] 2. AC2 action=revert by the resolver's lead / an executive / the auditor restores the exact archived row and logs a set; by anyone else it is refused (tests /RevertRestoresExactRowAndLogsSet, /RevertAuthority).
- [ ] 3. AC3 legacy resolve_conflict(key, chosen_value) returns the same result shape as main, and a new sibling write inserts knowledge_conflicts(kind=sibling) instead of writing conflict_with (tests /LegacyResolveConflictUnchangedForCallers, /SiblingWritesConflictRowNotConflictWith).
- [ ] 4. AC4 5 sampled audits with 2 reverted make that profile's next merge require a second concurrence; every archive on this path has invalidated_by (tests /AuditSampledAndPrecisionGateDowngrades, /NothingArchivedWithoutReversibleRecord).
- [ ] 5. AC5 TestToolSchemaBudget green with constants unchanged AFTER guard S2a (feat/guard-tool leaves 2130 B margin = 82 B above the 2048 headroom): the resolve_conflict extension must be net <= +82 B, offset by trimming descriptions in internal/relay/tools.go (names/params/types/enums unchanged, as in reclaim 55d8207); PR body gives before/after totals; go vet clean; go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

# [relay/knowledge] contradictions T2: resolve_conflict(action=claim|list|resolve|revert) with reversible invalidation

Task: 0090fabd-e5eb-4129-95f1-54f73565db75

ROOT_CAUSE: T1 (aa57add) detects knowledge conflicts but gives agents no way to act on them. A detected duplicate cannot be merged, so the fresh-key-vs-supersede gap stays open: the twin's consumers never hear about the change. Same-key siblings still write `memories.conflict_with` pointers outside the conflict model. Legacy `resolve_conflict(key, chosen_value)` archives irreversibly and records nothing that could be undone.

DECISION (design 8d107daa §4.3/§4.4/§5/§7 T2, ruling ed744dee: OQ5 fold, OQ6 revert authority, OQ7 escalation stays on the T1 ladder):
- **One tool, no new tools.** `resolve_conflict` gains `action=list|claim|resolve|revert` plus `conflict_id`, `resolution`, `keep` and `rationale`. Without `action`, the legacy key/chosen_value path runs byte-identically: same required-field errors, same `{resolved, memory}` result. The `task_edge(op=...)` multiplex pattern.
- **`db.ResolveKnowledgeConflict`** runs in one writer tx, lease holder only: CAS on `state='claimed' AND lease_holder=agent` and a lease that has not expired. Any refusal writes nothing. Per resolution:
  - `merge` / `supersede` archive every live member except `keep`.
  - `reject_new` archives the newest member.
  - Every archive sets `invalidated_by = <conflict id>` and writes one `knowledge_log` retract row with `prev_memory_id` (class retraction). That opens a coherence rollout to the archived memory's consumers, which closes the gap.
  - Edges are declared (`declared=1`, `rule=resolution:<id>`): `supersedes` for merge/supersede, `overrides` for `scope_split` / `override_declared`, `amends` for `both_valid_temporal`.
  - `both_valid_temporal` stamps `valid_until` on the older members and logs a validity row.
  - `not_a_conflict` sets `gate_evidence.distinct`; `members_hash` stays resolved, so the pair is never re-detected.
  - The prior state of every changed member (status, valid_until, archived_by, archived_reason) goes to `gate_evidence.prior`.
- **`db.RevertConflict`** is allowed for the resolver's lead (reports_to), any active executive, or an agent of the auditor profile. It restores every `invalidated_by=id` row exactly: `archived_*` back to prior, status back, `invalidated_by` kept for history, one `set` log row each. It also restores stamped windows (validity log row), deletes the `resolution:<id>` edges, and moves the conflict back to `detected` with `audit=reverted`. It is the only place that un-archives.
- **Sampled audits and the precision gate (§5).**
  - Sampling covers every archiving resolution: 1 in `contradiction_audit_rate` when set, else 1 in 3 while fewer than 30 audits have an outcome, then 1 in 10. The sequence lives in settings and the first archiving resolution is always sampled.
  - The auditor is the resolver's lead when that lead's profile differs, else an active executive of another profile.
  - A typed audit ticket (P2, dispatched by relay-sweeper) goes to the auditor profile. The auditor profile upholds by resolving the same way on the resolved conflict (`audit=upheld`) or reverts.
  - Outcomes are appended to `gate_evidence.audits`, keyed by the resolver's (profile, layer). Precision = upheld / (upheld + reverted) over 90 days.
  - Below `contradiction_precision_min` (0.8) over at least 5 audits, an archiving resolution becomes a **proposal**: nothing is archived, the conflict returns to `detected` with no failed claim counted, and `gate_evidence.proposal` is recorded. A second agent's matching claim and resolve concurs and archives (`concurred_by`); the proposer cannot concur with itself. Resolutions that archive nothing are never gated.
- **Sibling path (§4.4).** Both keep-both-live branches of `setMemoryTx` (based_on mismatch and upsert=false) insert or extend one `knowledge_conflicts(kind=sibling)` row in the same tx instead of writing `conflict_with`. An N-way group stays one row: extending re-hashes, which cannot collide because the new member is a fresh id. The returned memory still carries `ConflictWith`, so the `set_memory` result (`conflict`, `conflict_with`, `current_author`, the conflict notice) is unchanged.
- **Legacy wrapper.** `ResolveConflict` resolves the key's open sibling conflict as `supersede` keeping the chosen row. It opens that conflict row on the fly when the key has none, e.g. pre-T2 `conflict_with` pairs (0 rows in prod per design §1). Every archived alternative gets `invalidated_by` and a prior record, so it is revertable. The archive statement, the `opResolve` log row and the result are otherwise unchanged.
- **Contested flag.** `session_context.relevant_memories[].contested` is an omitempty bool, set for members of open conflicts: one RO query, no bytes on unaffected memories. `search_memory` / `list_memories` `conflict` stays true for post-T2 sibling rows (open sibling membership), so nothing reads as un-conflicted because `conflict_with` is no longer written. `MemoryStats` counts open sibling members too.

HOLD kept: non-destructive (invalidated_by plus a knowledge_log row on every archive; AC4 test). The write path is never refused (the sibling insert cannot collide). Key strings are never a G1 signal (detection untouched). The agent decides and detection only proposes.

Tool schema (AC5, cap and headroom constants unchanged; cto 8958ae7e: net <= +82 B):
- origin/main aa57add: 82 tools, **54714 B**, margin 2630.
- this branch: 82 tools, **54758 B**, margin 2586: **net +44 B**. resolve_conflict grew +185 B (action/conflict_id/resolution/keep/rationale; key/chosen_value no longer schema-required, the handler still refuses them missing on the legacy path). Offset −141 B by trimming 5 descriptions with no contract change: update_task, identity_check, is_eligible, reclaim_task, delete_team.
- merged with origin/feat/guard-tool (1c56b1c3, measured in a throwaway worktree, clean auto-merge): 83 tools, **55258 B**, margin **2086** >= 2048 headroom.

Files (9): internal/db/contradictions.go, internal/db/memories.go, internal/relay/handlers_memory.go, internal/relay/tools.go, internal/relay/project.go (MemorySummary.Contested), internal/relay/handlers.go (1 line: markContested at boot), and tests internal/db/contradictions_test.go, internal/relay/handlers_conflicts_test.go, internal/db/memories_causal_test.go. Three sibling assertions there read `conflict_with` off the stored row; they now assert the knowledge_conflicts pairing plus the unchanged `ConflictWith` on the returned memory (AC3 requires this change).

Verification: go vet ./... clean; go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... green.
- AC1: TestContradictionResolution/MergeArchivesWithInvalidatedByAndLogsRetraction. The loser is archived with invalidated_by=cid, the retract row has class retraction, the supersedes edge is declared, and EvaluateCoherence opens 1 rollout with a reassess obligation for the holder (carol). Also /OnlyLeaseHolderResolves: unclaimed, other agents, expired lease and bad keep are all refused; log head, archives and the conflict row are unchanged.
- AC2: /RevertRestoresExactRowAndLogsSet compares the whole row string before and after (new path, and the legacy path including valid_until). Also checks the set log row, deleted edges, and detected+reverted. /RevertAuthority: resolver, peer and non-exec are refused with nothing written; the executive may revert, so may another agent of the auditor profile; a second revert is refused.
- AC3: TestResolveConflictTool/LegacyResolveConflictUnchangedForCallers (result keys exactly `memory,resolved`, refusal texts unchanged). /SiblingWritesConflictRowNotConflictWith: no conflict_with written, one sibling row, 3rd and upsert=false members join it, MemoryStats=3, the legacy resolve closes it.
- AC4: /AuditSampledAndPrecisionGateDowngrades: 5 sampled audits (3 upheld, 2 reverted) give precision 0.6. The next merge is a proposal (nothing archived, no failed claim); self-concurrence is refused; bob's concurrence archives; the default early rate samples 2 of 6. /NothingArchivedWithoutReversibleRecord: merge, supersede, reject_new and legacy (winner and new-row) give 6 archives, all with invalidated_by plus a prior record.
- Extra: TestResolveConflictTool/ActionsListClaimResolveRevertAndContestedBoot runs the handler round trip and the boot contested flag on, off after resolve, and on again after revert.
- AC5: TestToolSchemaBudget green, numbers above.

## review-wraith verdict: SHIP
Scope: internal/db/contradictions.go (resolve/revert/audit/precision, sibling recorder), internal/db/memories.go (sibling branch, legacy wrapper, MemoryStats), internal/relay/handlers_memory.go (action multiplex, audit dispatch, contested), internal/relay/tools.go (schema + trims), internal/relay/project.go + handlers.go (contested flag), tests
Gate: build -tags fts5 OK / vet ./... OK / gofmt OK / test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK (db 159s, relay 167s)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- action=claim takes a conflict id without checking the caller's project, the same as T1's ClaimConflict. Resolve still requires the lease and list is project-scoped, so the only thing exposed is claiming a foreign conflict whose id you already know. A follow-up can pass the project into ClaimConflict.
- The web UI and CLI conflict badges read conflict_with, so post-T2 siblings show no badge there (API and MCP flags covered). UI is OUT per design §8.
- An upheld audit needs an explicit same-resolution resolve by the auditor profile; a sampled audit left alone counts toward neither side of precision.

Invariants checked: single writer (each resolve/revert/sibling write is one writerTx with a CAS on the conflict state; no new hot-path write; boot/search/list add one RO query each over open conflicts). No schema change: invalidated_by and both tables come from T1's migrations. memorySelectCols/agentColumns untouched. No new MCP tool; the schema cap and headroom constants are unchanged. The write path is never refused: the sibling insert/extend hash always includes a fresh id.

RED_EVIDENCE:
  cmd: go test -tags fts5 -count=1 -run 'TestContradictionResolution|TestResolveConflictTool' ./internal/db/ ./internal/relay/   (test files of ff7cfb4, non-test files reverted to origin/main aa57add)
  test_sha: ff7cfb4
  output: |
    # agent-relay/internal/db [agent-relay/internal/db.test]
    internal/db/contradictions_test.go:483:56: undefined: ConflictOutcome
    internal/db/contradictions_test.go:484:12: d.ResolveKnowledgeConflict undefined (type *DB has no field or method ResolveKnowledgeConflict)
    internal/db/contradictions_test.go:484:37: undefined: ConflictResolution
    internal/db/contradictions_test.go:519:42: undefined: ResolutionMerge
    internal/db/contradictions_test.go:556:83: undefined: ErrConflictNotHeld
    FAIL	agent-relay/internal/db [build failed]
    --- FAIL: TestResolveConflictTool (0.24s)
        --- FAIL: TestResolveConflictTool/ActionsListClaimResolveRevertAndContestedBoot (0.13s)
    FAIL	agent-relay/internal/relay	0.703s
    (TestResolveConflictTool/LegacyResolveConflictUnchangedForCallers passes on main: the legacy result shape is unchanged.)

## 3. Files changed

```
internal/db/contradictions.go             | 790 ++++++++++++++++++++++++++++++
 internal/db/contradictions_test.go        | 409 ++++++++++++++++
 internal/db/memories.go                   | 153 +++++-
 internal/db/memories_causal_test.go       |  41 +-
 internal/relay/handlers.go                |   1 +
 internal/relay/handlers_conflicts_test.go | 136 +++++
 internal/relay/handlers_memory.go         | 127 ++++-
 internal/relay/project.go                 |   3 +
 internal/relay/tools.go                   |  26 +-
 9 files changed, 1642 insertions(+), 44 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `0090fabd-e5eb-4129-95f1-54f73565db75`._
