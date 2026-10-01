# [relay/W8 D3] a PR merged event on a deploying task changes nothing

## Team : wraith-engine (tsukumo)
## Branch : wraith/55a8361c-pr-deploying-noop (from main)
## Relay task : 55a8361c-b4ef-40c7-8133-2a30273c1300
## Trace : trace=393f8042d3291c26bea783212b02335d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: merged webhook on a deploying task leaves status deploying
- [ ] 2. test: reconcile_pr(pr_state=merged) on a deploying task leaves status deploying and returns no error
- [ ] 3. test: merged webhook on an in-review task (non-pipeline) still moves it to done as today
- [ ] 4. go test -tags fts5 ./... green

## 2. Root cause & decisions

# 55a8361c W8 D3 PR merged on deploying = no-op

ROOT_CAUSE: internal/db/tasks.go ForcePRTransition (shared by the PR webhook webhook_pr_sync.go:82, reconcile_pr handlers_tasks.go:845 and the PR poll task_sweeper.go:79) only guarded done/cancelled (no-resurrect) and same-target (idempotent); a deploying task observed as merged was force-moved deploying -> done on the "user" path, skipping the post-merge deploy + verify.

FIX: ForcePRTransition returns (task, changed=false) for a deploying task on any PR observation (merged, late reopen). One guard covers all three callers. In-review merged -> done unchanged (AC3).

STACKED: built on D1 56e1ce15 (needs DeployTask); submitted after D1 merged.

## review-wraith verdict: SHIP
- no schema, no lock change, no tool schema change; pure early-return before the force transition
- covers webhook, reconcile_pr and sweeper poll through the one shared function
- full suite green via niwa slot (go test -tags fts5 ./...)

RED_EVIDENCE:
cmd: go test -tags fts5 ./internal/relay -run 'TestPRMerged|TestReconcilePrMergedLeavesDeploying'
test_sha: bdf3a09
output:
--- FAIL: TestPRMergedWebhookLeavesDeploying (0.20s)
    pr_deploying_test.go:39: merged webhook moved a deploying task to done, want deploying
--- FAIL: TestReconcilePrMergedLeavesDeploying (0.22s)
    pr_deploying_test.go:60: reconcile merged on a deploying task changed=true, want false
FAIL
FAIL	agent-relay/internal/relay	1.517s
FAIL

## 3. Files changed

```
internal/db/tasks.go                |  6 +++
 internal/relay/pr_deploying_test.go | 81 +++++++++++++++++++++++++++++++++++++
 internal/relay/webhook_pr_sync.go   |  2 +-
 3 files changed, 88 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `55a8361c-b4ef-40c7-8133-2a30273c1300`._
