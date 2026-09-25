# [split][wraith/db][integrity] name the slow scan class + Q4 orphan_agent_profile active-only + dangling-board no-target journaled once

## Team : wraith-engine (tsukumo)
## Branch : wraith/engine-27a77033 (from main)
## Relay task : 27a77033-71c2-4bdd-86cc-8a1e554dca10
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 slow-scan attribution: when RunReferentialScan takes >= writerSlowWait the log line names per-phase durations (read/apply/count/invariant) and the slowest class by name + duration; fast scans log nothing (unchanged). Unit test TestReferentialScanTimingNamesSlowestClass asserts the line format via the existing refScan log seam. PR body contains one real timing line captured from a local run against a read-only copy of ~/.agent-relay/_redeploy_bak/relay.db.pre-b559017-20260908T000022Z naming the class that eats the time.
- [ ] 2. AC2 orphan_agent_profile active-only (design Q4): class SQL adds status='active'; fixture with an inactive/deleted agent whose slug has no profiles row is NOT flagged, an active one still IS; an existing open row for a dead agent is stamped resolved_at by the next scan (heal path). Test TestOrphanAgentProfileActiveOnly.
- [ ] 3. AC3 dangling-board no-target journaled once: two consecutive SweepDanglingBoards(apply=true) passes over the same no-target task yield the disposition (and the relay journal line) exactly once; the second pass reports Scanned but zero new dispositions; the task row is byte-identical before/after (no delete, board_id untouched); re-homing or archiving the task clears the marker so a later recurrence is journaled again. Test TestDanglingBoardNoTargetJournaledOnce.

## 2. Root cause & decisions

ROOT_CAUSE: (1) the slow-scan line in RunReferentialScan logged only the total, so a 2-18s scan could not be attributed to a phase or a class. (2) orphan_agent_profile flagged every agents row with an unknown slug, including inactive/deleted agents, which can never pick up work (231 of 244 open rows). (3) a dangling-board no-target task stays a candidate forever and the sweep keeps no record of it, so every 2-minute sweep logged the same disposition again.
DECISION: (1) refDelta carries its class's read time; RunReferentialScan times read/apply/count/invariant; the >= writerSlowWait line is now 'integrity scan: took T (read R apply A count C invariant I; slowest class=<name> D; N classes)'. Fast scans stay silent. (2) orphan_agent_profile orphanSQL adds a.status = 'active'; the existing heal path (resolve = open \ orphan) stamps resolved_at on the dead-agent rows on the next scan, no data rewrite. (3) SweepDanglingBoards records each journaled no-target in integrity_quarantine (class dangling_board_no_target, row_id = task, ref_value = the dangling board_id) inside its existing writer tx and skips candidates already marked for the same board. The marker resolves when the task stops being that no-target candidate (re-homed, archived, board fixed); an upsert re-opens it when the problem comes back. Scanned still counts every candidate. The task row is never written; tasks is untouched by the marker path.
REJECTED: an in-memory seen-set in the relay sweeper (lost on restart, and task_sweeper.go was out of scope); MarkQuarantine for the marker (INSERT OR IGNORE cannot re-open a resolved row, and it is its own writer call outside the sweep tx).

EVIDENCE (read-only copy of ~/.agent-relay/_redeploy_bak/relay.db.pre-b559017-20260908T000022Z in worktree scratch, since deleted; writerSlowWait forced to 0 to print the line):
integrity scan: took 9ms (read 8ms apply 0s count 0s invariant 1ms; slowest class=orphan_dispatcher 1ms; 16 classes)
integrity scan: took 8ms (read 7ms apply 0s count 0s invariant 1ms; slowest class=orphan_profile 1ms; 16 classes)
UNCERTAIN: on an idle copy no class is slow (the whole scan takes 7-9ms). The 2-18s prod scans are not per-class query cost on this data; that points at live contention or I/O. The new line will name the phase and class on prod after the next redeploy.
AC2 open orphan_agent_profile on the copy: before 244 (active 13, inactive 50, deleted 181) -> after 13 (all active); 231 stamped resolved_at.

## review-wraith verdict: SHIP
Scope: internal/db/referential_integrity.go (timing + active-only), internal/db/dangling_board.go (no-target marker), and their two test files. No schema change: reuses integrity_quarantine and its UNIQUE key; a new class value only. No relay/ handler change.
Gate: build -tags fts5 OK / vet OK / gofmt OK (touched files; this also fixes the existing drift in referential_integrity_test.go) / test -tags fts5 -race ./internal/db/... ./internal/relay/... OK. The 3 new tests fail on main and pass here.
Checks: single writer kept (the marker reads run on d.ro() before the one existing sweep writer tx, and the marker writes run inside that tx); TestLiveScanUsesReaderAndWriterTx source-text anchor unchanged; the sweep never deletes a task and never blanks a board_id.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- The dangling_board_no_target open count now appears in the referential-scan summary line (intended, it is visible quarantine).

## 3. Files changed

```
internal/db/dangling_board.go             | 74 +++++++++++++++++++++++++-
 internal/db/dangling_board_test.go        | 73 ++++++++++++++++++++++++++
 internal/db/referential_integrity.go      | 41 ++++++++++++---
 internal/db/referential_integrity_test.go | 86 +++++++++++++++++++++++++++++--
 4 files changed, 262 insertions(+), 12 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `27a77033-71c2-4bdd-86cc-8a1e554dca10`._
