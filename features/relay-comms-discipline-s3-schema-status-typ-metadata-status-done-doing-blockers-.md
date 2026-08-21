# [relay] comms-discipline S3 — schema status typé metadata.status {done[],doing[],blockers[]} + valve note ≤280, accept-and-slot jamais reject

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/status-malformed-slot (from main)
## Relay task : d601ac41-881b-463c-b234-1984ab0f6272
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. metadata.status bien-forme accepte et stocke structure — test
- [ ] 2. status malforme/prose accepte via slot note/content, jamais 400 — test
- [ ] 3. blockers[] non-vide surface visiblement dans get_inbox sans declencher de wake — test
- [ ] 4. go build+test -tags fts5 ./... vert
- [ ] 5. invariant inbox non-destructive intact

## 2. Root cause & decisions

ROOT_CAUSE: send_status's done/doing/blockers args are read via mcp-go's `req.GetStringSlice(key, nil)`, which silently returns the default (nil) whenever the argument is present but not shaped as a string array — a bare prose string, a number, an array containing a non-string item. That data was simply discarded: accept-and-slot never rejected the call (no 400), but it also never preserved what the caller sent — same silent-default family already fixed today in c229fe44 (update_task) and 30a455fc (dispatch_task priority/board_id). d601ac41's AC2 ("status malforme/prose accepte via slot note/content, jamais 400") specifically requires the malformed input to land in note/content, not vanish.

DECISION: added `statusSlotArg(req, key)` (internal/relay/status.go) — reads the raw arg; a well-formed `[]any`/`[]string` slots normally (any non-string item inside the array is pulled out into prose rather than silently skipped); anything else (a bare string, a number, an object) is stringified as prose. `foldMalformedIntoNote(note, slot, prose)` appends a labeled `[slot: prose]` fragment onto the note when prose is non-empty (no-op otherwise). `HandleSendStatus` (internal/relay/handlers_messaging.go) now calls `statusSlotArg` for all three slots and folds any malformed prose into note before `buildStatusPayload` — which already caps/truncates note at 280 bytes, so no new cap logic was needed. Well-formed calls are byte-for-byte unaffected (empty prose → no-op fold).

The other 4 ACs (well-formed accepted+stored, blockers surfaced without waking, build/test green, inbox non-destructive-peek invariant) were already fully satisfied by the existing shipped mechanism (task 2b00cd82, merged 18509e0c) — verified against current `main` before touching anything (existing tests TestSendStatusNoWakeWithBlockers, TestBuildStatusPayloadSlotsAndRenders/Caps/Empty all already cover them). This task's actual delta is the malformed-slot gap above; no other code changed.

REJECTED ALTERNATIVE: re-implementing typed-status from scratch (the ticket description reads like a fresh S3 spec). Not pursued — the existing send_status mechanism already matches the design doc (DEC-relay-comms-discipline-1 §Mechanism B, Ruling-3) almost exactly; re-implementing would have duplicated shipped, tested code for no gain. Reconciled code-vs-doc first, found one real gap, fixed only that.

KNOWN UNRELATED FAILURE (do not attribute to this diff): `go test -tags fts5 ./internal/relay/... -run TestToolSchemaBudget` fails on the origin/main commit this branch is based on (56351/56320, 31 bytes over) — root-caused and fixed separately as task d7080bc2 (branch wraith-backend-2/schema-budget-trim, already qa-submitted). This diff touches zero tool schemas and does not move that byte count. Full suite here: 584 passed, 1 failed (that one, pre-existing).

Repro: internal/relay/handlers_send_status_test.go —
- TestSendStatusMalformedSlotFoldedIntoNote: done sent as a prose string → never a 400, done=[] in the stored metadata, the prose text survives verbatim in note.
- TestSendStatusStrayItemFoldedIntoNote: blockers=["real blocker", 3] → the well-formed string item still slots normally, the stray number is folded into note.
- Pre-existing TestSendStatusNoWakeWithBlockers / TestSendStatusP0StillWakes / TestSendStatusRequiresRecipient / TestBuildStatusPayload* all still pass unchanged.

## review-agent-runtime verdict: SHIP
Scope: internal/relay/status.go (statusSlotArg, foldMalformedIntoNote), internal/relay/handlers_messaging.go (HandleSendStatus wiring), internal/relay/handlers_send_status_test.go (2 new tests)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5: 584 passed, 1 failed (TestToolSchemaBudget, pre-existing on main, unrelated — see above; fixed separately as d7080bc2)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- §1-4, §6 n/a: no schema/scan touch, no concurrency/CAS touch (send_status was never a state-machine transition), no writer/hot-path write added (same single `InsertMessageWithDeliveries` call as before, just fed differently-sourced strings), no auth/route/lifecycle touch.
- §5: no tool schema change (send_status's registered params are unchanged — this is purely how the existing params are interpreted server-side), no identity-resolution bypass, no new contract field.

## 3. Files changed

```
internal/relay/handlers_messaging.go        | 19 ++++++---
 internal/relay/handlers_send_status_test.go | 65 +++++++++++++++++++++++++++++
 internal/relay/status.go                    | 49 ++++++++++++++++++++++
 3 files changed, 127 insertions(+), 6 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `d601ac41-881b-463c-b234-1984ab0f6272`._
