# [relay] send_message not-bound: queue-for-binding (or bindable-grace window) instead of hard-refuse

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/queue-for-binding (from main)
## Relay task : 0464d6cb-2e2b-4ab5-a22c-2cbdb384eedb
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
_(untyped ticket — no acceptance criteria)_

## 2. Root cause & decisions

> ⚠️ Root cause / arbitration not recorded by the doer yet. The gate requires it before merge — this gap is visible on purpose.

## 3. Files changed

```
internal/db/orgs.go                  | 16 ++++++++++
 internal/relay/handlers_messaging.go | 15 +++++++++-
 internal/relay/handlers_test.go      | 57 ++++++++++++++++++++++++++++++++++++
 3 files changed, 87 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `0464d6cb-2e2b-4ab5-a22c-2cbdb384eedb`._
