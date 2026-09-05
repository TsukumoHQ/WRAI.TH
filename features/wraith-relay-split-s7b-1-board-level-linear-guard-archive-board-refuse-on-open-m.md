# [wraith/relay][split] S7b-1: board-level Linear guard — archive_board refuse on OPEN mirrored tasks, delete_board refuse on ANY mirrored task, typed BOARD_HAS_LINEAR_TASKS, fail-closed

## Team : wraith-backend (tsukumo)
## Branch : wraith/boards-linear-guard (from main)
## Relay task : 9e0d14fb-783e-40f4-940b-e89bf5a1de58
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. board with ≥1 OPEN mirrored task (source='linear', archived_at NULL, status not done/cancelled): archive_board refused, code BOARD_HAS_LINEAR_TASKS, message has count + remedy, no task archived_at written (test)
- [ ] 2. board whose mirrored tasks are ALL terminal (done/cancelled) or archived: archive_board succeeds as today (test)
- [ ] 3. board referenced by ANY task with source='linear' (any status, archived included): delete_board refused with BOARD_HAS_LINEAR_TASKS + count; board row untouched (test)
- [ ] 4. native-only board: archive_board and delete_board behave exactly as today (test)
- [ ] 5. existence-check DB error: refusal (typed INTERNAL/unavailable), operation not performed — fail closed (test with a closed/failing db handle or injected error)
- [ ] 6. error payload = typed code + message with count; CategoryPermission, non-retryable (test asserts code + category)
- [ ] 7. go test -tags fts5 ./... green (count cited); diff ≤3 non-test files; zero schema change; TestToolSchemaBudget unchanged

## 2. Root cause & decisions

ROOT_CAUSE: Boards have no Linear coupling of their own, but two board ops silently desync Linear-mirrored tasks that sit on them. ArchiveBoard (internal/db/boards.go) cascades archived_at onto every task on the board (boards.go:110-113), so archiving a board stamps archived_at onto OPEN source='linear' rows and desyncs them from Linear. DeleteBoard leaves those tasks' board_id dangling (boards.go:121-130). Nothing refused either, so the fleet's Linear mirror could drift with no signal.

DECISION (DEC-wraith-boards-linear-guard-1, cto-tsukumo 2026-09-05 14:00Z; design trovex 25c1c9d8 §7 STATUS RULED): guard both ops fail-closed, inside the same writer tx as the cascade/delete, before it runs.
- archive_board: hard REFUSE when the board carries any OPEN mirrored task (source='linear' AND archived_at IS NULL AND status NOT IN ('done','cancelled')). No force flag. A board whose mirrored tasks are all terminal/archived archives freely — the cascade closing out a done mirror is not a desync.
- delete_board: REFUSE when ANY task (any status, archived included) still references the board via source='linear'.
- Typed error: new db.LinearTasksOnBoardError{Op,Count}; handler maps it to permissionError BOARD_HAS_LINEAR_TASKS (CategoryPermission, non-retryable) with the exact count+remedy message ('N open Linear-mirrored tasks on this board — move_task them off or close them in Linear first'; delete variant 'N Linear-mirrored tasks still reference this board').
- FAIL CLOSED: a query error on the existence check rolls back and returns the error; the destructive write never runs (NOT the fail-open shape of refuseIfArchived).
- Zero schema change (detection is a COUNT on existing columns); tools.go untouched (TestToolSchemaBudget green); native-only boards byte-identical to before.

REJECTED ALTERNATIVES:
- force flag on archive_board: ruling gives archive no escape hatch — the remedy is to move_task the mirror off or close it in Linear.
- RO-pool check then writerExec cascade: a TOCTOU window between check and write; ruling puts the check inside the same writer tx (single writer conn via beginWriterTx), so check+cascade are atomic.
- FK / schema constraint on board_id: out of scope and reverses the tolerate-dangling-board_id-for-native-tasks stance (Q4, separate P3). Dangling board_id for NATIVE tasks stays tolerated.
- Extending agentColumns / any schema migration: unnecessary; a COUNT over source/archived_at/status/board_id needs no new column.

```
## review-wraith verdict: SHIP
Scope: internal/db/boards.go (LinearTasksOnBoardError + ArchiveBoard/DeleteBoard rewritten to a writerTx guard), internal/relay/handlers_boards.go (typed-error mapping), internal/relay/boards_linear_guard_test.go (6 tests: AC1-4,6,7) + internal/db/boards_failclosed_test.go (AC5 fail-closed, r2).

R2 (round-1 reject, AC5 only): the AC5 test was tautological — a closed db handle makes every write fail, so it passed even with the guard removed. Replaced with a non-tautological injected error in the db package: drop the tasks table so the guard's COUNT errors while the boards DELETE would still succeed; DeleteBoard must refuse and leave the (archived) board row intact. Verified: guard present = PASS, guard fully removed = FAIL.
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (742 passed, 12 packages; TestToolSchemaBudget green).

BLOCKERS (must fix before merge):
- none. Single-writer intact: the guard runs on the one writer connection via beginWriterTx (no second writer); check+cascade/delete are one atomic tx (no TOCTOU, stronger than the prior two-writerExec form). Zero schema change; agentColumns/scanAgent untouched. No messaging/dispatch/deliveries/SSE/updater surface touched. Additive + backward-compatible: a COUNT over existing columns, native boards byte-identical to before.

NITS (non-blocking):
- none.
```

## 3. Files changed

```
...-linear-guard-archive-board-refuse-on-open-m.md |  65 +++++++
 internal/db/boards.go                              |  96 +++++++++--
 internal/db/boards_failclosed_test.go              |  40 +++++
 internal/relay/boards_linear_guard_test.go         | 187 +++++++++++++++++++++
 internal/relay/handlers_boards.go                  |  10 ++
 5 files changed, 385 insertions(+), 13 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-9e0d14fb-783e-40f4-940b-e89bf5a1de58
- 🟢 AC1: refusal + count/remedy + no archived_at on board or task all asserted — evidence: internal/db/boards.go:134-144 COUNT guard before cascade; handlers_boards.go:50-53 errors.As -> permissionError — test: TestArchiveBoardRefusesOpenLinearTasks internal/relay/boards_linear_guard_test.go:53 — FAILS when guard neutered (verified in /tmp copy)
- 🟢 AC2: asserts archive succeeds AND cascade stamps archived_at — non-vacuous — evidence: internal/db/boards.go:137 status NOT IN (done,cancelled) matches tasks.go:435, referential_integrity.go:123 — test: TestArchiveBoardAllowsTerminalLinearTasks internal/relay/boards_linear_guard_test.go:82
- 🟢 AC3: refusal with count asserted + board row still present — evidence: internal/db/boards.go:180-188 any-status COUNT before DELETE — test: TestDeleteBoardRefusesAnyLinearTask internal/relay/boards_linear_guard_test.go:103 — FAILS when neutered
- 🟢 AC4: native archive cascades + delete removes row as before — evidence: count=0 path byte-identical to prior; internal/db/boards.go:146-158 — test: TestNativeBoardArchiveDeleteUnchanged internal/relay/boards_linear_guard_test.go:125 + pre-existing TestBoardLifecycle (green)
- 🔴 AC5: tautological — closed handle makes fail-closed indistinguishable from fail-open; needs injected-error variant per AC allowance + DeleteBoard coverage — evidence: internal/db/boards.go:139-141 returns before UPDATEs (code correct), but test cannot observe — test: TestBoardGuardFailsClosedOnDBError internal/relay/boards_linear_guard_test.go:157 — PASSES with guard fully removed (executed in /tmp copy); asserts only IsError+CodeInternal; DeleteBoard untested
- 🟢 AC6: asserts code + errorCategory=permission + isRetryable=false — evidence: handlers_boards.go:52,69 permissionError -> errors.go:72 toolError(CategoryPermission,false) — test: TestBoardLinearGuardErrorEnvelope internal/relay/boards_linear_guard_test.go:179 — FAILS when neutered
- 🟢 AC7: all hygiene sub-conditions verified by execution — evidence: executed go build -tags fts5 (exit 0) + go test -tags fts5 -count=1 ./... — 8 pkgs ok, 657 PASS; 2 non-test code files + 1 doc; no schema.go change; tools.go untouched — test: TestToolSchemaBudget internal/relay — PASS, unchanged

## 5. Timeline

- round 1 → **reject** (review-9e0d14fb-783e-40f4-940b-e89bf5a1de58)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `9e0d14fb-783e-40f4-940b-e89bf5a1de58`._
