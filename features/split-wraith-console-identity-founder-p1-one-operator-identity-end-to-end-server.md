# [split][wraith/console-identity][founder P1] one operator identity end to end, server half: every guard/default that special-cases "user" also accepts "human"; "human" is the canonical operator

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/operator-identity-server (from main)
## Relay task : c5e67397-d09e-4d48-a0bd-a8f4a8b65904
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestOperatorReachableAsHuman: with teams configured in the project, POST /api/messages and MCP send_message from an agent to "human" succeed (message + delivery created) exactly like to "user"; to an unrelated agent still 403
- [ ] 2. AC2 TestOperatorForcePathHuman: TransitionTask with agentName "human" bypasses validTransitions exactly like "user" (e.g. done -> in-progress allowed); a regular agent name is still refused
- [ ] 3. AC3 TestOperatorDefaultsHuman: apiPostMemory without agent_name stores agent_name "human"; resolveTargets("human") == resolveTargets("user") unchanged; go test -tags fts5 ./internal/relay/... ./internal/db/... green; diff = exactly the 5 listed files

## 2. Root cause & decisions

# Decision — one operator identity, server half (task c5e67397)

## ROOT_CAUSE
#193 made the console SEND as "human", but the server still special-cased only
"user" in three reachability/authority spots, so under the new identity: an agent
could not answer the operator (direct-send reachability guards 403'd a send TO
"human" when teams exist), and the console acting as "human" lost the admin
force-path on task transitions. apiPostMemory also defaulted the writer to "user".

## FIX (5 files, exactly the ticket files line)
- internal/relay/api_messages.go: add `isOperator(name) = name=="user"||name=="human"`;
  REST direct-send guard bypasses for either identity.
- internal/relay/handlers_messaging.go: MCP send_message guard uses isOperator.
- internal/db/tasks.go: `transitionTask` force-path granted to "human" too
  (package db cannot import the relay helper, so the predicate is inlined).
- internal/relay/api.go: apiPostMemory default agent_name "user" -> "human".
- resolveTargets UNTOUCHED (already maps human/user -> ["user"], the read alias);
  "user" kept everywhere. No schema, no migration, no asset change.

## ACs
- AC1 TestOperatorReachableAsHuman: teams configured; bot-a -> "human" via REST
  POST /api/messages AND MCP send_message both succeed like "user"; -> unrelated 403.
- AC2 TestOperatorForcePathHuman: force done->in-progress as "human" allowed like
  "user"; a regular agent name refused.
- AC3 TestOperatorDefaultsHuman: apiPostMemory with no agent_name stored under
  "human"; resolveTargets("human")==resolveTargets("user").
- verify green: `go test -tags fts5 ./internal/relay/... ./internal/db/...` = 811 pass.
- diff = exactly the 5 listed files (rebased onto main 45090bf, no overlap).

## review-wraith verdict: SHIP
- Build bar: fts5/CGO suite green, 811 pass across relay+db.
- Single-writer / lock discipline: no DB writer or lock change; transitionTask body
  unchanged except widening the force-path predicate by one disjunct.
- agentColumns<->scanAgent lockstep: untouched.
- Task-transition TOCTOU / double-claim: force-path only skips validTransitions for
  the operator identities (admin), same as the pre-existing "user" behaviour — no new
  claim path, no CAS change.
- Non-destructive inbox: unchanged.
- Schema: no migration, no schema change.
- Blast radius: 3 predicate widenings (2 reachability guards + 1 force-path) + 1
  default flip; "user" preserved so no existing caller regresses; resolveTargets
  alias keeps delivery routing identical.

VERDICT: SHIP.

## 3. Files changed

```
internal/db/tasks.go                     |  7 +++-
 internal/relay/api.go                    |  2 +-
 internal/relay/api_messages.go           | 10 ++++-
 internal/relay/handlers_messaging.go     |  2 +-
 internal/relay/operator_identity_test.go | 70 ++++++++++++++++++++++++++++++++
 5 files changed, 86 insertions(+), 5 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c5e67397-d09e-4d48-a0bd-a8f4a8b65904`._
