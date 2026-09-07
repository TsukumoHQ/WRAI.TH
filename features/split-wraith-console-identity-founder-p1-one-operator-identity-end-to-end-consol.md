# [split][wraith/console-identity][founder P1] one operator identity end to end, console half: v1+v2 act as "human" and display operator traffic addressed to "user" OR "human"

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/console-identity (from main)
## Relay task : ea0cc293-1164-4a5f-a0c8-2f3a65d85388
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestConsoleOperatorSendsAsHuman: source scan of js/main.js + js/api-client.js + v2/api.js finds zero client-side literal "user" used as an actor/agent value (transition, cancel, dispatch, memory), and the operator constant is "human"
- [ ] 2. AC2 TestConsoleInboxAcceptsUserAndHuman: source scan asserts the v1 operator-inbox predicate (main.js isForUser) matches to === "user" AND to === "human"; and the same for the v2 inbox/thread filter if one exists
- [ ] 3. AC3 TestConsoleOtherAssetsUntouched: index.html, style.css, v2.css and every v2 asset not in the files line are byte-identical to main; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

ROOT_CAUSE: The web console hardcoded the operator actor as the literal "user" on every write path — v1 main.js task-card transitions (accept/done/cancel) + kanban onTransition default, api-client.js transitionTask default, and (missed by the ticket's double-quote-only FACTS grep) three single-quoted v2 sites: board.js:544 transition actor, messages.js operator identity `ME='user'`, memory.js add-directive default. So chatting/acting from the UI landed under the "user" profile instead of the intended single "human" operator identity, and the v1 operator inbox filtered on `msg.to === "user"` only, so agent replies addressed to "human" never surfaced.

DECISION: Unify on one operator identity, "human", across both consoles. v1: introduce `const OPERATOR = "human"` and use it for all four transition/cancel actor sends; api-client default actor -> "human"; widen isForUser to accept `msg.to` "user" OR "human" (legacy rows keep showing). v2: board.js/messages.js/memory.js actor + self-filter -> "human", with `isMe()` still recognising legacy 'user'. Pinned by a read-only source-scan Go test (no JS runtime under `go test`), precedent console_v2_config_panel_test.go.

REJECTED ALTERNATIVES:
- Fix only v1 + the ticket's literal "ONE v2 view file": leaves v2 kanban transitions and v2 memory writes still acting as "user", failing the founder "acts as human on every write" goal. Rejected; instead flagged the FACTS undercount to wraith-cto, who approved the widened 6-file plan (plan artifact approved 23:49:54Z).
- Flip the operator identity to "human" WITHOUT the legacy-accept in the inbox/self filters: would hide all pre-existing "user"-addressed history. Rejected — filters accept BOTH for backward-compat.

## review-wraith verdict: SHIP-WITH-NITS
Scope: console operator identity (ea0cc293) — internal/web/static/js/main.js, js/api-client.js, v2/board.js, v2/messages.js, v2/memory.js (embedded assets) + new internal/relay/console_operator_identity_test.go (source-scan). No relay engine/db/schema/deliveries/auth/MCP/updater code touched.
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 ./internal/relay/... ./internal/db/... OK (825 passed)

Fleet-backbone thesis: N/A — no sqlite writer, no schema/agentColumns/scanAgent, no dispatch/claim TOCTOU, no delivery/inbox change, no middleware/auth, no MCP tool, no self-updater. Client-side operator identity constant + receive-side filter widening only; the new Go test is a read-only embedded-asset scan.

Backward-compat: PRESERVED. v1 isForUser and v2 isMe() accept BOTH legacy "user" AND "human", so pre-existing "user"-addressed rows keep displaying. No migration, no wire/schema change.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- Depends on the server accepting actor/profile "human" on task transitions and /api/user-response — already true (server stamps "human" since #193; #152 kept "human" valid under the reject guard; task filters already accept profile_slug "human"), and the sibling server ticket hardens the guards. In-scope for that parallel ticket, not this console half.
- Scope note (cto-approved): fixes 3 v2 view files (board/messages/memory) vs the ticket's literal "ONE v2 view file" line — the FACTS section undercounted single-quoted 'user' in v2. Widened to meet the founder "acts as human on every write" goal; plan artifact approved by wraith-cto 23:49:54Z.

## 3. Files changed

```
internal/relay/console_operator_identity_test.go |  88 +++++++++++++++++++++++
 internal/web/static/js/api-client.js             |   2 +-
 internal/web/static/js/main.js                   |  16 +++--
 internal/web/static/v2/board.js                  |   2 +-
 internal/web/static/v2/memory.js                 | Bin 9188 -> 9189 bytes
 internal/web/static/v2/messages.js               |  10 +--
 6 files changed, 106 insertions(+), 12 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ea0cc293-1164-4a5f-a0c8-2f3a65d85388`._
