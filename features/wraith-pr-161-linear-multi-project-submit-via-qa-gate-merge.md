# [wraith] PR #161 linear multi-project — submit via qa gate + merge

## Team : wraith-backend (tsukumo)
## Branch : feat/linear-multi-project (from main)
## Relay task : d1422119-e8b6-4cf8-85da-4a7aadb62fec
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. branch feat/linear-multi-project rebased onto fresh origin/main before submit
- [ ] 2. qa-submit run with THIS task id, git_branch/git_worktree/git_target stamped on review transition
- [ ] 3. gate approves and daemon merges; PR #161 shows merged
- [ ] 4. build/test/vet -tags fts5 + CGO green post-rebase

## 2. Root cause & decisions

> ⚠️ Root cause / arbitration not recorded by the doer yet. The gate requires it before merge — this gap is visible on purpose.

## 3. Files changed

```
internal/connector/linear/backfill.go    |  14 +++-
 internal/connector/linear/linear_test.go | 125 +++++++++++++++++++++++++++++++
 internal/connector/linear/reconcile.go   |  32 +++++---
 internal/connector/linear/webhook.go     |  60 +++++++++++++--
 internal/relay/api.go                    |   2 +
 internal/relay/linear_manager.go         |  33 +++++++-
 internal/web/static/v2/v2.js             |  10 ++-
 7 files changed, 253 insertions(+), 23 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `d1422119-e8b6-4cf8-85da-4a7aadb62fec`._
