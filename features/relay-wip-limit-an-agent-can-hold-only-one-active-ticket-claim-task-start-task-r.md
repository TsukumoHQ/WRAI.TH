# [relay WIP limit] an agent can hold only ONE active ticket: claim_task/start_task refused while it already has one accepted or in-progress (in-review doesn't count)

## Team : wraith-engine (tsukumo)
## Branch : wraith-engine/e2273dc3-wip-limit (from main)
## Relay task : e2273dc3-c506-405d-8321-ea9336e4fafb
## Trace : trace=a722260e6547e2c6aac08d048ab08d57
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. claim_task by an agent holding one accepted/in-progress task -> refused WIP_LIMIT naming the held task (test)
- [ ] 2. claim_task next same refusal (test)
- [ ] 3. a task in in-review does not count: after review_task the doer can claim the next (test)
- [ ] 4. a bounced task back to in-progress counts again (test)
- [ ] 5. wip_limit is a project setting, default 1, value 0 = unlimited (test)
- [ ] 6. dispatcher force override allowed and written to task history (test)
- [ ] 7. report command/endpoint lists agents currently over the limit
- [ ] 8. go test -tags fts5 ./... green

## 2. Root cause & decisions

# e2273dc3 — WIP limit: one active ticket per agent

ROOT_CAUSE: claim_task / start_task only checked the task's own state edge; nothing counted what the caller already held, so agents sat on 2-3 accepted tickets for hours (founder 19:40Z).

## Decision
- projects.wip_limit (default 1, 0 = unlimited), PATCH /api/projects/{name} {"wip_limit": N}.
- checkWIP in ClaimTask / StartTask (db): count accepted|in-progress with lease_holder = caller; in-review excluded; WIP_LIMIT names held ids.
- Start of an already-held task not re-checked; override actor starting someone else's task (niwa resume) not counted (lease stays with the worker, S3 rule); bounced task counts again for the doer.
- force on claim_task only (dispatcher / human / RELAY_OVERRIDE_ACTORS), audited wip_override; REST forced accept uses the same path.
- GET /api/wip?project= report (no auto-release).

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db (wip_limit.go, tasks.go, db.go), internal/relay (api.go, handlers_tasks.go, tools.go), skill/tools-reference.md, fixture lifts in 7 tests
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (8 packages)
BLOCKERS: none
NITS:
- The count check runs before the claim CAS (not inside it): two concurrent claims by the SAME agent could both pass. One agent racing itself is rare; a CAS-embedded count would need transitionTaskCode surgery.
- Fleet upgrade: default 1 applies at once; agents already over it keep their tasks (report lists them) but cannot claim more until they submit.

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/db/ -run TestWIPClaimAndStartRefusedWhileHolding -count=1
  test_sha: d7a6f4d
  output: |
    (run on origin/main a50b809 with the test's main-compatible core)
    --- FAIL: TestWIPClaimAndStartRefusedWhileHolding (0.05s)
        zz_red_test.go:13: claim while holding one: want WIP_LIMIT, got nil (worker-a now holds two active tasks)
    FAIL
    FAIL	agent-relay/internal/db	0.339s

## 3. Files changed

```
internal/db/db.go                           |   3 +
 internal/db/exceptions_test.go              |   1 +
 internal/db/guards_test.go                  |   1 +
 internal/db/notification_rules_test.go      |   1 +
 internal/db/org_edges_test.go               |   1 +
 internal/db/task_lease_test.go              |   2 +
 internal/db/tasks.go                        |  17 +++
 internal/db/wip_limit.go                    | 176 +++++++++++++++++++++++
 internal/db/wip_limit_test.go               | 215 ++++++++++++++++++++++++++++
 internal/relay/api.go                       |  43 +++++-
 internal/relay/guards_test.go               |   5 +
 internal/relay/handlers_consumption_test.go |   5 +
 internal/relay/handlers_tasks.go            |   6 +-
 internal/relay/tools.go                     |   6 +-
 internal/relay/wip_limit_test.go            | 132 +++++++++++++++++
 skill/tools-reference.md                    |   2 +-
 16 files changed, 607 insertions(+), 9 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `e2273dc3-c506-405d-8321-ea9336e4fafb`._
