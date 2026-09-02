# [wraith/relay] P3: gate apiPostUserResponse (7th dedupHit-discarding site, missed by 1a7e18b1)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/dedup-footgun-userresponse (from main)
## Relay task : e60624d9-62d2-430b-ab35-68ca57fa2620
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. internal/relay/api.go:878 apiPostUserResponse gates Registry.Notify on !dedupHit
- [ ] 2. no behavior change today (dedupHit is always false there) — verified by existing test suite staying green
- [ ] 3. a short comment records the GetMessage(nil,nil)-on-deleted-row nil-check gotcha for future idempotency_key work on any of these 7 sites
- [ ] 4. build/vet/gofmt/test -tags fts5 green

## 2. Root cause & decisions

> ⚠️ Root cause / arbitration not recorded by the doer yet. The gate requires it before merge — this gap is visible on purpose.

## 3. Files changed

```
internal/db/messages.go | 10 +++++++++-
 internal/relay/api.go   | 12 +++++++++---
 2 files changed, 18 insertions(+), 4 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `e60624d9-62d2-430b-ab35-68ca57fa2620`._
