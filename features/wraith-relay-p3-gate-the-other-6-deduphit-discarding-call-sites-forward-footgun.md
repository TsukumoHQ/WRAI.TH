# [wraith/relay] P3: gate the other 6 dedupHit-discarding call sites (forward-footgun)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/dedup-footgun-fix (from main)
## Relay task : 1a7e18b1-daa9-411a-8061-055b5a51be05
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. all 6 flagged call sites gate their Notify/NotifyBroadcast/notifyConversation and events.Emit(Type:message) calls on !dedupHit, matching HandleSendMessage's pattern
- [ ] 2. no behavior change today (dedupHit is always false at these sites since none pass idempotency_key) — verified by existing test suite staying green
- [ ] 3. build/vet/gofmt/test -tags fts5 green

## 2. Root cause & decisions

> ⚠️ Root cause / arbitration not recorded by the doer yet. The gate requires it before merge — this gap is visible on purpose.

## 3. Files changed

```
internal/relay/api_messages.go       | 31 +++++++++++++++++++------------
 internal/relay/federation.go         | 14 ++++++++++----
 internal/relay/handlers.go           | 35 ++++++++++++++++++++---------------
 internal/relay/handlers_messaging.go | 35 +++++++++++++++++++++--------------
 internal/relay/notifications.go      | 10 ++++++++--
 5 files changed, 78 insertions(+), 47 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1a7e18b1-daa9-411a-8061-055b5a51be05`._
