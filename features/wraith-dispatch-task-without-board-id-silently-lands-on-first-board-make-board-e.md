# [wraith] dispatch_task without board_id silently lands on first board — make board explicit-or-refuse

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/board-explicit-or-refuse (from main)
## Relay task : 30a455fc-a329-4a52-824c-5b811ef8bdb6
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. dispatch_task on a multi-board project without board_id returns an error naming the available boards (id + slug), task NOT created
- [ ] 2. dispatch_task on a single-board project without board_id still works (unambiguous default preserved, no fleet-wide breakage)
- [ ] 3. board_id accepts slug as well as uuid if cheap — else error message shows both so callers can copy one
- [ ] 4. audit of other task-creation paths (Linear reconcile, HTTP API) documented in the PR: each either requires a board, has a deterministic documented route, or is fixed the same way
- [ ] 5. tests cover: multi-board omitted (error), multi-board explicit (ok), single-board omitted (ok)

## 2. Root cause & decisions

ROOT_CAUSE: dispatchCore's board-resolution block (internal/relay/handlers_tasks.go) picked `boards[0]` whenever board_id was omitted and at least one board already existed — `boards[0]` is the OLDEST board (ListBoards orders by created_at), so on any project with more than one board an omitted board_id silently mis-filed the task onto whichever board happened to be created first, not any deliberate default. On tsukumo this landed ~18 tasks on the Trovex board over 3 days before anyone noticed. Separately, dispatch_task's priority argument was read with `req.GetString("priority", "P2")`, and mcp-go's GetString silently returns the default whenever the key is present but not a string (e.g. the JSON integer 1) — so a wrong-typed priority landed P2 with no signal at all (repro: task 7670216a, msg 9839c920).

DECISION:
- internal/db/tasks.go: added `BoardRequiredError{Project, Boards}` (same shape/pattern as the existing `TypedTicketError`) and a board-resolution guard inside `DispatchTask` itself — the single choke every creation path funnels through (dispatchCore for MCP/cron/webhook, and the REST + batch paths which call `DispatchTask` directly). boardID==nil: 0 boards → left nil (caller's own auto-create, e.g. dispatchCore's "Backlog" board, already ran first); 1 board → unambiguous implicit default, unchanged; >1 boards → refused, message names every board as `slug (id)`.
- internal/relay/handlers_tasks.go dispatchCore: removed the `else boardID = &boards[0].ID` branch — board resolution for 1-or-more boards is now entirely `DispatchTask`'s job, never a silent pick in the handler layer. Zero-board auto-create-"Backlog" behavior is unchanged.
- internal/relay/handlers_tasks.go HandleUpdateTask... (unrelated, not touched) — HandleDispatchTask: (1) surfaces `*db.BoardRequiredError` via `errors.As` as a validation error; (2) the existing board_id UUID-prefix resolver now also matches by slug (board_id accepts slug as well as uuid — AC #3), so a caller can copy either column straight out of the refusal message; (3) priority is now read from the raw MCP argument map instead of `GetString`: if the key is present, it must be a string AND one of P0/P1/P2/P3, else refused (`CodeInvalidArgument`) naming the accepted values — omitted still defaults to P2 unchanged.
- internal/relay/api.go apiDispatchTask (REST): surfaces `*db.BoardRequiredError` the same way (400 + message) since it calls `DispatchTask` directly and now gets the same guard for free. NOT given the MCP path's board_id auto-create-on-zero-boards convenience — REST never had that (board_id stays nil), unchanged, documented as a known asymmetry, not a regression.
- internal/relay/tools.go dispatchTaskTool(): the tool description claimed "Without board_id, assigned to the first board" — now describes the real contract (0/1 boards auto-assigned, >1 refused with the list). Fits inside `TestToolSchemaBudget`'s 56320-byte cap (verified).
- HandleBatchDispatchTasks: no code change needed — it already calls `db.DispatchTask` directly per item and surfaces any error via `%v` into the `errors` array, so `*db.BoardRequiredError` and the priority path (batch decodes `Priority string` via `encoding/json`, which already rejects a JSON number into a string field at unmarshal time — no separate fix needed there) both work per-item, batch continues (not all-or-nothing). Covered by TestBatchDispatchTasks_MultiBoard_OmittedBoardIDRefusedPerItem.

AUDIT of every task-creation path (AC #4):
- dispatch_task (MCP, HandleDispatchTask → dispatchCore → db.DispatchTask): fixed, same rule.
- cron.go scheduled dispatch (→ dispatchCore): boardID always nil today — gets the same rule for free; a cron schedule on a multi-board project will now refuse until board_id is added to the schedule config (acceptable: cron schedules are operator-authored, not a live-agent hot path).
- webhook_signal.go inbound-signal dispatch (→ dispatchCore): boardID comes from webhook config (cfg.BoardID) — same rule applies when that's unset.
- apiDispatchTask (REST, → db.DispatchTask directly): same rule, fixed.
- HandleBatchDispatchTasks (→ db.DispatchTask directly, per item): same rule, fixed, no code change needed (generic error passthrough already existed).
- Linear reconcile mirror: grepped for board_id / DispatchTask usage — the Linear connector does not call dispatch_task/DispatchTask at all (it syncs status/fields on already-created tasks via its own reconcile loop), so there is no board-selection fallback to fix there.

REJECTED ALTERNATIVE: accepting board slug/uuid resolution in batch and REST too (parity with the MCP path's prefix+slug lookup). Not pursued — out of the reported scope (the production incident was specifically MCP dispatch_task), and batch/REST already work with a full UUID; noted as a NIT, not a blocker.

Repro: internal/relay/dispatch_board_priority_test.go —
- TestDispatchTask_MultiBoard_OmittedBoardIDRefused: 2 boards, board_id omitted → refused naming both slugs, task NOT created.
- TestDispatchTask_SingleBoard_OmittedBoardIDStillWorks: 1 board, board_id omitted → still lands there (implicit default preserved).
- TestDispatchTask_MultiBoard_ExplicitBoardIDWorks: naming the board by full UUID or by slug both dispatch normally.
- TestBatchDispatchTasks_MultiBoard_OmittedBoardIDRefusedPerItem: one item named, one omitted → 1 dispatched, 1 in errors (not all-or-nothing).
- TestDispatchTask_PriorityWrongType_Refused: priority as JSON number → refused, names P0..P3, task NOT created.
- TestDispatchTask_PriorityInvalidString_Refused: priority="urgent" → refused.
- TestDispatchTask_PriorityValidString_StillWorks: priority="P1" persists; omitted still defaults to P2.

## review-agent-runtime: PASS
Scope: internal/db/tasks.go (BoardRequiredError, DispatchTask board guard), internal/relay/handlers_tasks.go (dispatchCore simplification, HandleDispatchTask board/priority validation), internal/relay/api.go (apiDispatchTask error surfacing), internal/relay/tools.go (dispatchTaskTool description), internal/relay/dispatch_board_priority_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (580 passed, 12 packages)

BLOCKERS: none

NITS (non-blocking):
- No schema/scan change (§1 n/a). No new writer, no per-request write on a hot path (§3): the added `ListBoards` call inside `DispatchTask` is a read through `d.ro()`, only on the CREATE path, not a state transition.
- §2 concurrency: `DispatchTask`'s board guard is a plain read-then-decide, not a CAS — this is a CREATE (new row, unique UUID), not a transition on an existing row, so there's no double-claim/lost-update hazard the way there is for claim/start/complete. A benign race exists only if a board is created concurrently between the `ListBoards` read and the INSERT (task lands on whichever board set was visible at read time, or the count changes from 1→2 mid-flight and the caller sees a refusal it didn't expect on a retry) — cosmetic, not data-corrupting, not worth a transaction for a human-authored board-creation event.
- Batch dispatch and REST don't get the MCP path's UUID-prefix/slug board_id lookup convenience — pre-existing asymmetry (batch/REST already required a full UUID before this change), not a regression; flagged as a possible follow-up, not fixed here (kept to the reported scope).

## 3. Files changed

```
internal/db/tasks.go                           |  38 +++++
 internal/relay/api.go                          |   5 +
 internal/relay/dispatch_board_priority_test.go | 183 +++++++++++++++++++++++++
 internal/relay/handlers_tasks.go               |  34 ++++-
 internal/relay/tools.go                        |   2 +-
 5 files changed, 255 insertions(+), 7 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `30a455fc-a329-4a52-824c-5b811ef8bdb6`._
