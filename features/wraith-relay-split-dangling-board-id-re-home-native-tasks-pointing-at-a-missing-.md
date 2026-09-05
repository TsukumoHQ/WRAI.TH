# [wraith/relay][split] Dangling board_id re-home: native tasks pointing at a missing/archived board re-homed via ProductBoardSlugForProfile, never deleted, dry-run default ON, one journal line per task, idempotent

## Team : wraith-backend (tsukumo)
## Branch : wraith/dangling-board (from main)
## Relay task : 56d64e55-1580-4e5f-a107-73c6af51ad8d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 dry-run (default, no env, no setting): a native task whose board_id references a missing board and one whose board is archived are both reported action=rehome with the ProductBoardSlugForProfile target; zero rows change (test asserts board_id unchanged)
- [ ] 2. AC2 apply (setting dangling_board_apply='1' or env): both tasks get board_id = the active product board of their project; from/to journaled one line per task; a second pass reports 0 candidates (idempotence test)
- [ ] 3. AC3 never delete: task rows, archived_at and status untouched by the sweep; a candidate whose project has no active target board stays as is with action=no-target (test)
- [ ] 4. AC4 scope: source='linear' tasks with a dangling board_id are NOT candidates (test); archived tasks (archived_at set) are NOT candidates (test)
- [ ] 5. AC5 go test -tags fts5 ./... green (count cited); zero schema change; TestToolSchemaBudget unchanged; ≤4 files incl. scribe md; PR body pastes the live-DB count query above and the expected dispositions (16 rows: 15 tsukumo -> tsukumo 'backlog' or per-profile board, 1 trovex analytics-lead -> 'backlog' if that board exists, else no-target)

## 2. Root cause & decisions

# 56d64e55 — dangling board_id re-home (Q4 residual)

ROOT_CAUSE: a NATIVE task can keep a board_id that points at a board which was
hard-deleted (DeleteBoard) or archived (ArchiveBoard leaves terminal/other rows'
pointers, and a board can be archived out from under a done task). The board
guards (DEC-wraith-boards-linear-guard-1) only protect source='linear' rows from
being stranded; a native task's dangling pointer is TOLERATED, never repaired.
Nothing else converges it either: the lease sweep only requeues expired leases,
the limbo sweep only quarantines dead-assignee tasks, and reconcile owns only
linear rows. So the task shows under no live board — the Q4 residual S7b-1 left
open. Live DB (2026-09-05, read-only) count query:

    SELECT t.project, t.status,
           CASE WHEN b.id IS NULL THEN 'missing' ELSE 'archived' END, COUNT(*)
    FROM tasks t LEFT JOIN boards b ON b.id = t.board_id
    WHERE t.source='native' AND t.board_id<>''
      AND (b.id IS NULL OR b.archived_at IS NOT NULL)
    GROUP BY 1,2,3

= 16 rows: tsukumo/done/missing 15, trovex/in-progress/missing 1 (profile
analytics-lead → slug 'backlog'). Archived-board case = 0 today, in scope anyway.

DECISION (cto-tsukumo brief #3, design-lite wraith-cto 2026-09-05 15:30Z):
re-home, NEVER delete or blank board_id. A periodic sweep (db.SweepDanglingBoards)
finds each dangling native task and moves it onto its profile's ACTIVE product
board resolved by ProductBoardSlugForProfile — the SAME lookup the dispatch-time
auto-router and the one-time backfill use, so the mapping cannot drift. A task
whose target product board does not exist in its project is a 'no-target'
disposition: left exactly as is (never invents a board, never blanks the
pointer). Scope: source='native' AND archived_at IS NULL AND board_id<>'' AND
board missing-or-archived; ANY task status (done rows too, so board views stay
coherent). source='linear' rows are NEVER touched (reconcile owns their
board_id); archived tasks are out of scope.

Dry-run is the DEFAULT (safe first deploy): every disposition is computed and
journaled, zero rows written, until an operator opts in — env
RELAY_DANGLING_BOARD_APPLY (1/true/yes) wins, else the DB setting
dangling_board_apply='1'. This mirrors limboSweepApply exactly. The sweep rides
the existing 2-minute integrity ticker next to sweepLimboAssignees — no new
goroutine, no hot-path write. One journal line per disposition
(`integrity: dangling-board action=… mode=… task=… project=… from=… to=… slug=…`)
plus a summary line; NO digest messages (a re-home is not something a dispatcher
needs pinged about). Idempotent: after an apply pass every task points at an
active board, so the candidate query matches nothing on a re-run.

Single-writer discipline: the candidate cursor is fully drained/closed and every
target board is resolved on the RO pool BEFORE a single writer tx applies all
re-homes; the `board_id = FromBoard` guard on each UPDATE makes a concurrent move
win instead of being clobbered (RowsAffected 0 = benign raced no-op).

REJECTED ALTERNATIVES:
- Blank the dangling board_id (set NULL): loses the audit of where the task was
  and still leaves it unhomed; the ruling is explicit — re-home, never blank.
- Delete the orphaned task: inverts the immutable-audit / tolerate-don't-cascade
  posture (DEC-wraith-referential-integrity-phase3-1); nothing is ever deleted.
- Reuse backfillProductBoardRouting: it is a one-time boot migration over ACTIVE
  (non-done/cancelled) tasks and re-homes ANY mis-boarded task; this sweep is the
  narrower, periodic, dangling-only case that must also cover done rows and stay
  dry-run-gated. Shares only the ProductBoardSlugForProfile lookup + the
  `slug + archived_at IS NULL` target query, deliberately.
- A new MCP tool / new setting surface: none added — an operator flips the
  existing-style DB setting or env, exactly as A1-ii's limbo sweep. Zero
  tool-schema growth (TestToolSchemaBudget unchanged).

ZERO schema change; ≤4 files (dangling_board.go, dangling_board_test.go,
task_sweeper.go, + scribe md).

## Migrated / covered dispositions (AC1–AC4)

| AC | test | asserts |
|----|------|---------|
| AC1 | TestSweepDanglingBoardsDryRun | missing + archived board tasks → action=rehome, correct slug/target; zero rows changed |
| AC2 | TestSweepDanglingBoardsApplyIdempotent | apply moves both to the active product board; second pass = 0 candidates |
| AC3 | TestSweepDanglingBoardsNeverDeleteNoTarget | no active target → action=no-target; row/status/archived_at/board_id all intact |
| AC4 | TestSweepDanglingBoardsScope | source='linear' and archived tasks are NOT candidates (unchanged) |
| AC5 | TestToolSchemaBudget | schema/budget unchanged (76 tools) |

## review-wraith verdict: SHIP
Scope: internal/db/dangling_board.go (new), internal/db/dangling_board_test.go (new), internal/relay/task_sweeper.go — periodic re-home of native tasks off a missing/archived board_id. No schema, no MCP tool, no auth/inbox/updater/ingest surface touched.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (772 passed, 12 pkgs).

BLOCKERS (must fix before merge):
- none. Single-writer discipline held: candidate scan + all target-board lookups run on the RO pool and the cursor is drained/closed BEFORE one beginWriterTx applies the re-homes; per-UPDATE `board_id = FromBoard` guard makes a concurrent move win (RowsAffected 0 = benign no-op). No schema change (agentColumns/scanAgent untouched); TestToolSchemaBudget unchanged (76 tools). Not a hot-path write — rides the existing 2-min integrity ticker. source='linear' never touched; archived tasks out of scope; never deletes or blanks a board_id.

NITS (non-blocking):
- SweepDanglingBoards applies all re-homes in a single tx; at fleet scale (16 rows today) this is fine, but a future very-large candidate set would hold the single writer connection for the whole batch. Acceptable now; chunk if the candidate count ever grows unbounded.

## 3. Files changed

```
internal/db/dangling_board.go      | 164 +++++++++++++++++++++++++++++
 internal/db/dangling_board_test.go | 210 +++++++++++++++++++++++++++++++++++++
 internal/relay/task_sweeper.go     |  44 ++++++++
 3 files changed, 418 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `56d64e55-1580-4e5f-a107-73c6af51ad8d`._
