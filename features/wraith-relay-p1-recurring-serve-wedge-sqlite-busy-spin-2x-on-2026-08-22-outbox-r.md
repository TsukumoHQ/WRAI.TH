# [wraith/relay] P1: recurring serve wedge (sqlite busy-spin, 2x on 2026-08-22) + outbox replay dupes + claim_task/get_inbox writes not landing

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/serve-wedge-fix (from main)
## Relay task : 34037526-b601-49b6-bac0-ecd8273b3889
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. Root cause of the wedge identified with symbolized stack or reproduced test (not speculation)
- [ ] 2. Writer-lock leak path fixed + regression test (wedge repro or lock-leak unit test)
- [ ] 3. Busy-handler spin bounded: hard timeout -> error response, never infinite spin behind a leaked lock
- [ ] 4. Duplicate-delivery replay root-caused and fixed (ack path verified under write-lock contention) + test
- [ ] 5. claim_task/get_inbox timeout-not-landing class retested after fix and confirmed gone
- [ ] 6. Relay survives a soak: concurrent send/claim/inbox load without HTTP stall

## 2. Root cause & decisions

> ⚠️ Root cause / arbitration not recorded by the doer yet. The gate requires it before merge — this gap is visible on purpose.

## 3. Files changed

```
internal/db/agents.go             |  24 ++++----
 internal/db/audit.go              |   4 +-
 internal/db/boards.go             |   8 +--
 internal/db/conversations.go      |  12 ++--
 internal/db/custom_events.go      |   4 +-
 internal/db/db.go                 |  69 +++++++++++++++++++++-
 internal/db/decisions.go          |   2 +-
 internal/db/deliveries.go         |  14 ++---
 internal/db/elevations.go         |   6 +-
 internal/db/events.go             |  12 ++--
 internal/db/file_locks.go         |   6 +-
 internal/db/linear.go             |  20 +++----
 internal/db/memories.go           |  13 ++---
 internal/db/messages.go           |  10 ++--
 internal/db/notification_rules.go |  10 ++--
 internal/db/orgs.go               |  14 ++---
 internal/db/profiles.go           |   6 +-
 internal/db/projects.go           |  10 ++--
 internal/db/quotas.go             |   4 +-
 internal/db/runs.go               |   2 +-
 internal/db/skills.go             |  12 ++--
 internal/db/task_lease.go         |   4 +-
 internal/db/task_progress.go      |   4 +-
 internal/db/tasks.go              |  50 ++++++++--------
 internal/db/token_usage.go        |   6 +-
 internal/db/watchdog.go           |   2 +-
 internal/db/wedge_test.go         | 119 ++++++++++++++++++++++++++++++++++++++
 internal/relay/relay.go           |  25 ++++++--
 internal/relay/serve_test.go      |  53 +++++++++++++++++
 main.go                           |  71 +++++++++++++----------
 30 files changed, 431 insertions(+), 165 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `34037526-b601-49b6-bac0-ecd8273b3889`._
