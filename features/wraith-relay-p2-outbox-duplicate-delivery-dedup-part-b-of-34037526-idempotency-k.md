# [wraith/relay] P2: outbox duplicate-delivery dedup (part B of 34037526) — idempotency key

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/outbox-idempotency-key (from main)
## Relay task : ac328091-c46b-45c5-bda3-9afc760642e6
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. caller-supplied idempotency key threaded through send_message -> InsertMessageWithDeliveries (or equivalent dedup key)
- [ ] 2. duplicate send with the same idempotency key short-circuits to the existing message/delivery, no new delivery_id
- [ ] 3. heartbeat/status-resend and other legitimate identical-content-without-a-key traffic is unaffected (existing 5 tests stay green)
- [ ] 4. regression test reproducing a client-retry-after-timeout scenario
- [ ] 5. build/test/vet -tags fts5 + CGO green, gofmt clean, review-wraith verdict in PR body

## 2. Root cause & decisions

ROOT_CAUSE: Outbox retries (client timeout, then a resend of the same logical
message) had no way to be told apart from a genuine new send. InsertMessageWithDeliveries
always inserted a fresh message + deliveries, so a retried send produced 3-5
DISTINCT delivery_ids for the same logical message (observed: backend-lead-2
"STOPPED" x5, trovex-backend deploy-blocked x3, stale-qa-notification-replay
x3). Part A (34037526, merged 2ade5fec) fixed the writer-pool wedge that
triggered client timeouts in the first place; this task (part B) adds the
caller-side fix so a retry that DOES land is idempotent regardless of cause.

Fix: optional caller-supplied idempotency_key threaded through send_message ->
InsertMessageWithDeliveries (variadic trailing param, zero changes to the other
10 existing call sites). Inside the same writer tx, a non-empty key is looked
up against (project, from_agent, idempotency_key) BEFORE insert; a hit
short-circuits to the existing message (no new row, no new deliveries) instead
of duplicating. No key (the default) never dedups, so legitimate
identical-content resends (heartbeats, repeated status) are unaffected.

## review-wraith verdict: SHIP
Scope: internal/db/db.go (additive idempotency_key column + partial index),
internal/db/messages.go (InsertMessageWithDeliveries dedup, same writer tx),
internal/relay/handlers_messaging.go (idempotency_key param, both call sites),
internal/relay/tools.go (send_message schema param), internal/relay/toolsize_test.go
(budget 56320->57344, genuine new surface per file's own precedent),
internal/db/messages_idempotency_test.go (new), internal/relay/handlers_messaging_idempotency_test.go (new).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (629 passed, 0 failed)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/relay/handlers_messaging.go:164,192 — on a dedup-hit (retry with
  same idempotency_key), registry.Notify/NotifyBroadcast still fires and
  AddToTeamInbox still runs (idempotent via INSERT OR IGNORE, harmless there).
  The SSE/wake notify is NOT idempotent — a retry still pushes a second wake
  event to the recipient, even though no new delivery/inbox row is created.
  Out of this task's acceptance criteria (which only requires no new
  delivery_id + inbox count unaffected, both verified by test) but worth a
  follow-up if duplicate wakes (not duplicate inbox entries) resurface after
  this ships.

## 3. Files changed

```
internal/db/db.go                                  | 12 ++++
 internal/db/messages.go                            | 37 +++++++++++-
 internal/db/messages_idempotency_test.go           | 69 ++++++++++++++++++++++
 internal/relay/handlers_messaging.go               |  5 +-
 .../relay/handlers_messaging_idempotency_test.go   | 69 ++++++++++++++++++++++
 internal/relay/tools.go                            |  1 +
 internal/relay/toolsize_test.go                    | 15 ++---
 7 files changed, 196 insertions(+), 12 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ac328091-c46b-45c5-bda3-9afc760642e6`._
