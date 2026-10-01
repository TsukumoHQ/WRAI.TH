# [relay/W10] a send to an inactive non-service agent tells the sender nobody is reading

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/w10-recipient-inactive (from main)
## Relay task : dd6b1ab3-d3b6-4891-a085-5e3a1724666b
## Trace : trace=d9636337286411ee8bae48c398380c15
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: send_message to an inactive non-service agent delivers and the response carries warning recipient_inactive with name and last_seen
- [ ] 2. test: send to an active agent carries no warning
- [ ] 3. test: ack/fyi-type send, and send to an is_service recipient, carry no warning
- [ ] 4. go test -tags fts5 ./... green

## 2. Root cause & decisions

# dd6b1ab3 — W10: a send to an inactive non-service agent tells the sender nobody is reading

ROOT_CAUSE: the T2 liveness gate (toolset.go, `guardIdentity` → `db.SenderEligibility`) checks only the SENDER. `HandleSendMessage` rejects only a recipient that is unknown or deleted. Its comment notes that 'sleeping'/'inactive' agents are re-bindable and that their delivery queues. So a send to an inactive agent returned a plain success, and the sender never learned that nobody would read it (field probe ce21438c, 11:17:47Z).

DECISION:
- Delivery is unchanged. The inbox stays non-destructive, and a reactivated agent reads its mail.
- The send response now carries `warning: {code: "recipient_inactive", name, status, last_seen, message}` when the direct recipient matches all of these:
  - registered;
  - not `is_service`;
  - not active or sleeping (the same liveness rule as the sender gate, via `db.SenderEligibility`).
- No warning is added in these cases:
  - `ack`/`fyi` sends (they need no reader);
  - service recipients;
  - team, broadcast, conversation and operator (`user`/`human`) sends;
  - a fleet-expected recipient that is not registered yet (a boot race; it will register).
- A plain send keeps its exact prior JSON shape. `sendResult` adds `warning` only when it is set; `reply_to_resolved` is unchanged.
- Rejected: refusing the send. The ticket requires the message is never dropped or refused, and a typed error would make clients retry or park a message that was actually delivered.
- Not in scope: the cross-project path (`sendCrossProject`), whose target must be an executive or come through an arm; the warning can be added there later if needed.

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/ -run 'TestSendToInactive|TestSendToActive|TestSendInactive' -count=1
  test_sha: 3a87d3f
  output: |
    --- FAIL: TestSendToInactiveRecipientWarns (0.09s)
        recipient_inactive_test.go:57: want warning recipient_inactive {name carol, status, last_seen}, got <nil>
    FAIL
    FAIL	agent-relay/internal/relay	0.832s
    FAIL

## review-wraith verdict: SHIP
Scope:
- internal/relay/handlers_messaging.go (`HandleSendMessage` recipient capture, `sendResult`, `recipientInactiveWarning`)
- tests: internal/relay/recipient_inactive_test.go

Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 ./... OK (all 8 packages, run through niwa slot, on f6256ae)

BLOCKERS (must fix before merge):
- none.
  - No new query: it reuses the recipient row that the unknown-recipient guard already reads.
  - No write.
  - Delivery is never refused, and the success shape is unchanged when there is no warning.

NITS (non-blocking):
- none.

## 3. Files changed

```
internal/relay/handlers_messaging.go      | 53 +++++++++++++++----
 internal/relay/recipient_inactive_test.go | 88 +++++++++++++++++++++++++++++++
 2 files changed, 132 insertions(+), 9 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `dd6b1ab3-d3b6-4891-a085-5e3a1724666b`._
