# [relay/routing] a doer can always message its dispatcher and the lead that pinged it: gate-lead-2 -> niwa-cto-2 never FORBIDDEN for lack of a reply-path

## Team : wraith-engine (tsukumo)
## Branch : wraith/be29e23f-doer-reach (from main)
## Relay task : be29e23f-6dc5-4bd8-917e-226c83af2950
## Trace : trace=356b43135335aa824d6f619c89698bb2
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: B dispatched an active task to A -> A can message B
- [ ] 2. test: B messaged A twice -> A can reply to both (reply-path not single-use)
- [ ] 3. test: an unrelated agent is still refused, and the FORBIDDEN body names the rule

## 2. Root cause & decisions

# be29e23f — a doer always reaches its dispatcher and its reports_to chain

ROOT_CAUSE: internal/db/orgs.go CanMessage allowed same team, DIRECT reports_to only, notify channel, elevation, or a reply-path = a live delivery target->sender. Prod DB (read-only, 2026-10-05): gate-lead-2 reports_to 'cto', no team shared with niwa-cto-2, yet niwa-cto-2 dispatched it 4 open tasks. Its dispatch/ping messages TTL-expire (4h) and PurgeExpiredMessages deletes their deliveries, so the reply-path vanishes and gate-lead-2 -> niwa-cto-2 is FORBIDDEN. The relay reply-path itself is multi-use while the delivery lives ("single-use" is niwa status wording); AC2 test was green before the fix and pins it.

FIX: CanMessage adds (1) dispatcher route: target dispatched sender a task not done/cancelled; (2) reports_to walk up sender's whole chain (recursive CTE, depth <= 8, cycle-safe). Both send-path refusals return FORBIDDEN via noRouteError with message prefix "send.no_route:" and body field rule=send.no_route. Read-only queries only, reached only after the team checks fail; no schema change.

NOT DONE (needs ruling if wanted): reply-path from already-purged messages via message_tombstones — tombstones lack metadata.cross_project, so it would reopen the W2 R3 homonym hole.

RED_EVIDENCE:
  cmd: niwa slot run -- go test -tags fts5 ./internal/relay -run 'DoerCan|ReplyPathServes|UnrelatedSendRefused' -count=1
  test_sha: cd84acb (rebased from 98ba297)
  output: --- FAIL: TestDoerCanMessageActiveTaskDispatcher: doer -> dispatcher of its active task refused: {"code":"FORBIDDEN",...}
          --- FAIL: TestDoerCanMessageUpItsReportsToChain: doer -> its manager's manager refused
          --- FAIL: TestUnrelatedSendRefusedNamingRule: refusal does not name rule send.no_route
          (TestReplyPathServesEveryMessage = AC2 guard, green before and after)

## review-wraith verdict: SHIP
Scope: internal/db/orgs.go (CanMessage), internal/relay/handlers_messaging.go (2 refusals), internal/relay/errors.go (noRouteError), internal/relay/send_route_test.go
Gate: vet -tags fts5 OK / gofmt OK / test -tags fts5 ./... OK (on c521829)
BLOCKERS: none
NITS: dispatcher route is one-way (doer -> dispatcher); dispatcher -> doer already passes via admin team in prod.
Migration note: none.

## 3. Files changed

```
internal/db/orgs.go                  |  38 +++++++---
 internal/relay/errors.go             |  10 +++
 internal/relay/handlers_messaging.go |   4 +-
 internal/relay/send_route_test.go    | 134 +++++++++++++++++++++++++++++++++++
 4 files changed, 176 insertions(+), 10 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `be29e23f-6dc5-4bd8-917e-226c83af2950`._
