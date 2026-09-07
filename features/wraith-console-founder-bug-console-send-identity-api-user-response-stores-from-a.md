# [wraith/console][founder bug] console send identity: /api/user-response stores from_agent "user", UI/filters expect "human" — make the server send as human

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/console-human (from main)
## Relay task : 83396f2f-44c3-4c07-8711-caea59008f35
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestUserResponseSenderIsHuman: POST /api/user-response -> stored message from_agent == "human" and a delivery row exists for the target agent
- [ ] 2. AC2 TestUserAliasStillRouted: a message addressed to "user" and one addressed to "human" resolve to the same inbox target (notifications target resolution) and isAgent("human") == false unchanged
- [ ] 3. AC3 TestV1V2AssetsUntouched: internal/web/static/js/api-client.js, js/main.js, v2/api.js byte-identical to main; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

# Decision — console send identity (task 83396f2f)

## ROOT_CAUSE
`apiPostUserResponse` (internal/relay/api.go) hard-coded the sender as `"user"`
at both the `InsertMessageWithDeliveries` insert and the `Registry.Notify` push.
The console's human-profile filter and the v2 UI key off `from == "human"`, so a
message the founder typed in the console arrived tagged `user` and fell outside
the human profile. The client sends NO sender field (server-authoritative), so
the fix is server-side only: emit `"human"` at both call sites.

## FIX
- api.go apiPostUserResponse: sender arg `"user"` -> `"human"` at the
  InsertMessageWithDeliveries call and the Registry.Notify call. Nothing else.
- `resolveTargets` already maps both `"human"` and `"user"` -> `["user"]`
  (notifications.go:520), so routing/wake is unchanged — no notifications.go edit.

## SCOPE
2 files: internal/relay/api.go, internal/relay/console_identity_human_test.go.
Client assets (static/js/*, static/v2/*) untouched — pinned by AC3.

## AC3 note
Recorded plan asserted all THREE assets contain `/api/user-response`. main.js
does NOT reference the endpoint (it routes through api-client.js); asserting it
would false-fail. AC3 implemented to intent: the two real callers
(static/js/api-client.js, static/v2/api.js) still POST `/api/user-response` and
send no `sender`; main.js pinned present+non-empty. Byte-identity still enforced
by the gate diff-scope check.

## review-wraith verdict: SHIP
- Build bar: `go test -tags fts5 ./internal/relay/...` green — 483 pass (480 -> +3).
- Single-writer / lock discipline: no DB-layer change; insert path unchanged
  except the literal sender value. No new writer.
- agentColumns<->scanAgent lockstep: untouched.
- Task-transition TOCTOU / double-claim: N/A (no task lifecycle change).
- Non-destructive inbox: unchanged (same InsertMessageWithDeliveries call, one
  arg value flipped).
- Schema: no migration, no schema change.
- Blast radius: one string literal at two call sites on the console reply path;
  `resolveTargets` alias keeps `human`==`user` routing.

VERDICT: SHIP.

## 3. Files changed

```
internal/relay/api.go                         |  4 +-
 internal/relay/console_identity_human_test.go | 96 +++++++++++++++++++++++++++
 2 files changed, 98 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `83396f2f-44c3-4c07-8711-caea59008f35`._
