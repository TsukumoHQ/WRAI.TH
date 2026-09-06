# [wraith/relay][split] ack_delivery returns NOT_FOUND on nonexistent message_id (silent-success bug)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/ack-notfound (from main)
## Relay task : 12512c6e-0817-4a65-ad2a-c061c740595d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 ack_delivery with a message_id matching no delivery and no message returns a typed NOT_FOUND error naming the id — test
- [ ] 2. AC2 ack_delivery with a valid unread message_id still acknowledges and returns the same success shape as before — existing tests green unmodified
- [ ] 3. AC3 cross-project id (message exists in another project) also refuses — test
- [ ] 4. AC4 go build ./..., go vet ./..., go test -tags fts5 ./... green (count cited); ≤2 non-test source files; TestToolSchemaBudget total unchanged

## 2. Root cause & decisions

ROOT_CAUSE: ack_delivery's message_id fallback (handlers_messaging.go HandleAckDelivery) called db.AcknowledgeDeliveryByMessage, which runs an UPDATE ... WHERE message_id=? AND to_agent=? AND project=? AND state IN ('queued','surfaced'). A bogus id matches zero rows, but writerExec returns nil error on a 0-row UPDATE, so the handler echoed {acknowledged_message_id: <bogus>} — a silent success. Live repro 2026-09-05 22:35Z: acked 'ffbf98ef-2c3a-4bfe-95cf-c3e43d72ac7f' (no such message), relay reported success, the real unread stayed unread, the caller believed the inbox drained (founder silent-failure theme).
FIX: AcknowledgeDeliveryByMessage now checks RowsAffected; on 0 it runs a project-scoped EXISTS over deliveries+messages to distinguish a real id whose delivery is already acked (idempotent success) from an id that resolves NOTHING in the caller's project (bogus, or a message only in another project) — returning a typed *db.MessageNotFoundError. The handler maps that to validationError(CodeNotFound, ...) naming the id. Valid ack path (RowsAffected>0) is byte-for-byte unchanged; no schema/tool-schema change (TestToolSchemaBudget still 51647 B).

## review-wraith verdict: SHIP
Scope: internal/db/deliveries.go (AcknowledgeDeliveryByMessage: 0-row guard + typed MessageNotFoundError), internal/relay/handlers_messaging.go (map to validationError NOT_FOUND, +errors import), internal/relay/ack_notfound_test.go (new, 3 handler tests). 2 non-test source files.
Gate: build -tags fts5 OK / vet OK / gofmt OK / golangci-lint 0 issues / test -tags fts5 OK (811 passed, was 808 +3 new).

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none. Deliveries/inbox invariants (§5) preserved: non-destructive peek unchanged, valid ack path byte-identical, no second writer, no schema/PRAGMA/tool-schema change, existence check on the RO pool. The only behavior delta is refusing an id that resolves nothing — the intended fix.

## 3. Files changed

```
internal/db/deliveries.go            |  43 +++++++++++++-
 internal/relay/ack_notfound_test.go  | 107 +++++++++++++++++++++++++++++++++++
 internal/relay/handlers_messaging.go |   8 +++
 3 files changed, 156 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `12512c6e-0817-4a65-ad2a-c061c740595d`._
