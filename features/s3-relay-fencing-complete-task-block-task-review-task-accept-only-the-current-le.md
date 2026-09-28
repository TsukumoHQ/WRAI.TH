# [S3 relay fencing] complete_task / block_task / review_task accept only the current lease holder with the current lease generation; a replaced or swept worker cannot publish

## Team : wraith-engine (tsukumo)
## Branch : wraith-engine/0b980988-lease-fencing (from main)
## Relay task : 0b980988-ae01-409c-a717-20f47e35bb16
## Trace : trace=462e8d03a4eab3fad9da40c05a039ae6
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. complete/block/review from a non-holder or stale generation -> refused with a named error (test)
- [ ] 2. reclaim/transfer bumps the generation (test)
- [ ] 3. dispatcher override path audited in the task history (test)
- [ ] 4. F-03: /api/* rejects Host other than localhost/127.0.0.1:<port> and any foreign Origin; state-changing /api/* require Content-Type application/json; CORS middleware never passes a disallowed origin to the handler (tests)
- [ ] 5. F-04/F-27 relay half MOVED to S3b 05525713 (niwa-cto split 18:41Z, cto-tsukumo approved 19:21Z): not graded here
- [ ] 6. go test -tags fts5 ./... green

## 2. Root cause & decisions

# S3 0b980988 — relay lease fencing + F-03 /api hardening

ROOT_CAUSE: transitionTaskCode (internal/db/tasks.go) validated only the state edge, never the caller: any agent could complete/block/review any task (pending->done legal, no holder check), and nothing distinguished a replaced or swept worker from the current one, so a stale worker could overwrite mission state. Separately, /api/* trusted any Host, any Origin (corsMiddleware passed disallowed origins to the handler) and any Content-Type, so a cross-site or DNS-rebinding browser page could drive state changes on the loopback relay.

## Decision (rulings from niwa-cto, msgs 64881cb9 / 0709636d)
- tasks.lease_generation INTEGER NOT NULL DEFAULT 0 (additive, ensureColumns); +1 on claim / claim_next / reclaim / reassign / update_task transfer; every transition UPDATE, reclaim and ReassignTaskFields CAS on the generation read.
- complete/block/review (MCP, batch, REST) fenced to the lease holder; generation checked when supplied, required under RELAY_STRICT_FENCING=1 (ambiguity A pick b). Refusal = TASK_LEASE_FENCED, non-retryable, nothing written.
- Override = human/user, dispatcher, RELAY_OVERRIDE_ACTORS (default "niwa", the gate daemon); is_service alone is NOT enough. Audited action=lease_override. Override resume keeps the lease with the prior holder / assignee.
- Linear mirrors (no relay claim) use the assignee as holder.
- F-03: /api/* Host must be localhost/127.0.0.1/[::1] at the listen port or RELAY_ALLOWED_HOSTS (421); state-changing /api/* need application/json (415); corsMiddleware refuses (403) any Origin that is neither same-host nor listed, on every path. Signed inbound (federation, GitHub/signal/Linear webhooks) exempt.
- F-04/F-27 (per-agent token) SPLIT to its own ticket per niwa-cto: this branch closes ACs 1,2,3,4,6.

RESIDUAL: until RELAY_STRICT_FENCING=1, a same-name respawn omitting lease_generation passes the holder check (niwa-side follow-up filed by niwa-cto).

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db (tasks.go, task_lease.go, db.go, models), internal/relay (handlers_tasks.go, tools.go, middleware.go, api_guard.go, relay.go), internal/config, web UI js (4 fetch headers), docs/deployment.md
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK (all 8 packages)
BLOCKERS: none
NITS:
- Upgrade behaviour change: a relay reached on a LAN IP / public name (UI or hooks via RELAY_URL) gets 421 on /api/* until RELAY_ALLOWED_HOSTS lists it; a browser MCP client (e.g. inspector) on /mcp gets 403 until RELAY_CORS_ORIGINS lists its origin. Documented in docs/deployment.md; worth a release-note line.
- ReassignTask bumps the generation without a CAS (it had no CAS before); applyLeaseOnTransition stays a separate post-CAS write (pre-existing).
- scripts/team-*.sh POST /api/spawn/children/*/kill with no Content-Type — endpoint no longer exists, untouched.

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/db/ ./internal/relay/ -run 'TestFenceNonHolderTerminalWritesRefused|TestCORSNeverPassesDisallowedOrigin' -count=1
  test_sha: 4d7f9c1
  output: |
    (run on origin/main 052b1ec with the two tests' main-compatible core; full set added in 4d7f9c1 / 1dbbeef)
    --- FAIL: TestFenceNonHolderTerminalWritesRefused (0.04s)
        zz_red_test.go:10: complete by non-holder worker-b: want TASK_LEASE_FENCED, got nil (task published by a non-holder)
    FAIL
    FAIL	agent-relay/internal/db	0.324s
    --- FAIL: TestCORSNeverPassesDisallowedOrigin (0.00s)
        zz_red_test.go:17: foreign origin https://evil.test reached the handler
    FAIL
    FAIL	agent-relay/internal/relay	0.391s

## 3. Files changed

```
docs/deployment.md                                 |  11 +-
 ...-task-review-task-accept-only-the-current-le.md |  91 +++++++
 internal/config/config.go                          |  17 +-
 internal/db/board_routing_test.go                  |   3 +
 internal/db/db.go                                  |   5 +
 internal/db/notification_rules_test.go             |   3 +
 internal/db/task_fence_test.go                     | 294 +++++++++++++++++++++
 internal/db/task_lease.go                          |  84 +++++-
 internal/db/tasks.go                               | 115 ++++++--
 internal/models/task.go                            |   4 +
 internal/relay/api_guard.go                        |  88 ++++++
 internal/relay/api_guard_test.go                   | 140 ++++++++++
 internal/relay/handlers_tasks.go                   |  50 +++-
 internal/relay/handlers_tasks_test.go              |   5 +-
 internal/relay/middleware.go                       |  28 +-
 internal/relay/relay.go                            |  15 +-
 internal/relay/task_fence_handler_test.go          |  72 +++++
 internal/relay/tools.go                            |   9 +-
 internal/web/static/js/api-client.js               |   4 +-
 internal/web/static/js/notifications.js            |   4 +-
 20 files changed, 967 insertions(+), 75 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-0b980988-ae01-409c-a717-20f47e35bb16
- 🟢 AC1: Behavioral fence confirmed at DB and handler layer; named error TASK_LEASE_FENCED surfaced; row untouched. — evidence: internal/db/task_lease.go:91-145 fencedStatus + checkLeaseFence refuse non-holder / stale generation with CodeTaskLeaseFenced; internal/db/tasks.go:770-795 transitionTaskCode invokes checkLeaseFence for done/blocked/in-review; handlers call CompleteTaskFenced/BlockTaskFenced/ReviewTaskFenced. — test: internal/db/task_fence_test.go TestFenceNonHolderTerminalWritesRefused (line 47) refuses complete/block/review by non-holder; TestFenceStaleGenerationRefused (line 91) refuses stale generation; TestFenceUnheldPendingDoneRefused (line 75) refuses worker on unheld. Handler-level mirror: internal/relay/task_fence_handler_test.go TestTerminalToolsFencedByLeaseGeneration exercises MCP handlers end-to-end.
- 🟢 AC2: Every ownership-grant path bumps generation by exactly one; CAS guarantees no double-bump on lost race. — evidence: internal/db/task_lease.go:215 ReclaimTask SET lease_generation = lease_generation + 1 AND lease_generation = ? CAS; internal/db/tasks.go:1817 ReassignTask bumps; internal/db/tasks.go:1910 ReassignTaskFields bumps. ensureColumns at internal/db/db.go:960 adds lease_generation INTEGER NOT NULL DEFAULT 0 (additive, legacy-safe). — test: internal/db/task_fence_test.go TestLeaseGenerationBumpsOnEveryGrant (line 137) asserts g()==1 after claim, g()==2 after reclaim, g()==3 after transfer (ReassignTaskFields), g()==4 after reassign (ReassignTask); also asserts ClaimNextTask yields generation 1.
- 🟢 AC3: Override path audited; non-allowlisted is_service agents are fenced like any other. — evidence: internal/db/task_lease.go:152 auditLeaseOverride writes AuditEntry{Action: lease_override, Reason: override}; internal/db/tasks.go:991-993 calls it when override && fencedStatus(newStatus). isOverrideActor at internal/db/task_lease.go:101-120 whitelists human / user / DispatchedBy / RELAY_OVERRIDE_ACTORS (default niwa); is_service alone does NOT qualify. — test: internal/db/task_fence_test.go TestFenceOverrideAudited (line 218) iterates {dispatcher, human, niwa}; for each, an is_service agent is refused first, then the override actor completes and ListAudit returns exactly one lease_override row. Also tests RELAY_OVERRIDE_ACTORS env knob and that a holder publishing its own task writes zero lease_override rows.
- 🟢 AC4: Each F-03 behavior has a behavioral test proving the handler did not run on rejection. — evidence: internal/relay/api_guard.go:34-47 hostAllowed loopback-at-port or RELAY_ALLOWED_HOSTS returns 421; Content-Type application/json required on POST/PUT/PATCH/DELETE returns 415; corsMiddleware refuses (403) any Origin that is not same-host and not listed, applied on every path. Signed inbound paths exempt via signedInboundPaths. Middleware order CORS->RateLimit->BodyLimit->Auth->APIGuard->handler (internal/relay/relay.go:200-210). — test: internal/relay/api_guard_test.go TestAPIGuardRejectsForeignHost covers loopback-at-port and RELAY_ALLOWED_HOSTS accept, foreign host and port-mismatch reject 421, non-/api paths pass; TestAPIGuardRequiresJSONOnStateChange covers POST/PUT/PATCH/DELETE with empty/text/plain/form-urlencoded/multipart reject 415, application/json (+charset) accept, GET unaffected; TestCORSNeverPassesDisallowedOrigin covers foreign origin rejected 403 (with and without RELAY_CORS_ORIGINS), same-origin and no-Origin pass; foreign origin on /mcp also rejected.
- 🔴 AC5: Nothing in the diff addresses this criterion. The ticket body says the AC was split to a separate ticket, but the AC list still names it; per dispatcher rules this is RED unless covered by a pre-existing test, which it is not. Gate should reconcile the AC list against the ticket body's stated scope (ACs 1,2,3,4,6). — evidence: internal/relay/middleware.go:14-61 authMiddleware is the only auth gate; no per-agent token binding logic in this diff or pre-existing; internal/relay/settings_spec.go:90 defaults RELAY_TRUST_LOOPBACK to 1 with no auto-disable for niwa-managed installs; grep for niwa-managed install returns only the AC line itself. The ticket body at features/s3-relay-fencing-complete-task-block-task-review-task-accept-only-the-current-le.md:31 explicitly carves F-04/F-27 to a separate ticket per niwa-cto. — test: NONE - no test in this diff (or pre-existing) covers per-agent token as/from binding or auto-disabling RELAY_TRUST_LOOPBACK for niwa-managed installs. The pre-existing TestAuthMiddleware_LoopbackExemption and TestAuthMiddleware_TrustLoopbackDisabled only exercise the env-var behaviour in isolation, not the niwa-managed condition.
- 🟢 AC6: Hard CI gate green. — evidence: executed: go build -tags fts5 ./... (Success), go vet -tags fts5 ./... (No issues), go test -tags fts5 ./... (1340 passed in 12 packages). — test: full suite - 1340/1340 ok across 12 packages including the new task_fence_test.go (294 lines) and api_guard_test.go (140 lines) and task_fence_handler_test.go (72 lines).

## 5. Timeline

- round 1 → **reject** (review-0b980988-ae01-409c-a717-20f47e35bb16)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `0b980988-ae01-409c-a717-20f47e35bb16`._
