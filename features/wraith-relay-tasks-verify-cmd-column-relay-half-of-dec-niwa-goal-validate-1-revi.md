# [wraith/relay] tasks.verify_cmd column — relay half of DEC-niwa-goal-validate-1 (reviewer /goal validate command)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/verify-cmd-column (from main)
## Relay task : 866b13f2-84e3-4e94-9e4d-a0fa819593e0
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. Additive nullable tasks.verify_cmd migration, auto-runs on startup, idempotent, older binary still reads the DB
- [ ] 2. dispatch_task + batch_dispatch_tasks + update_task accept verify_cmd; dispatcher-only + audited like goal/AC/dod
- [ ] 3. get_task / list_tasks / get_session_context return it
- [ ] 4. Typed-ticket guard: verify_cmd absent = warn at most, never refuse
- [ ] 5. No relay-side validation of command content; -tags fts5 suite green incl migration re-run test

## 2. Root cause & decisions

> ⚠️ Root cause / arbitration not recorded by the doer yet. The gate requires it before merge — this gap is visible on purpose.

## 3. Files changed

```
internal/db/db.go                 |  10 +++
 internal/db/tasks.go              |  48 ++++++-----
 internal/db/verify_cmd_test.go    | 108 +++++++++++++++++++++++++
 internal/models/task.go           |   9 +++
 internal/relay/api.go             |   2 +-
 internal/relay/handlers_tasks.go  |  18 +++--
 internal/relay/tools.go           |   6 +-
 internal/relay/verify_cmd_test.go | 164 ++++++++++++++++++++++++++++++++++++++
 8 files changed, 338 insertions(+), 27 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `866b13f2-84e3-4e94-9e4d-a0fa819593e0`._
