# [relay/learning] guards S2b (resubmit id): ladder match_precedent evaluates guards, human generalize compiles a shadow guard, hourly SweepGuards

## Team : wraith-engine (tsukumo)
## Branch : feat/guards-ladder (from main)
## Relay task : d17673fc-fe90-411e-ae3b-02e9a5b387f8
## Trace : trace=51b6ca269f82b28cc6ef3274ae07b6c7
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 a matching ACTIVE ladder guard resolves the systemic at match_precedent with resolved_by=precedent and no ask_source message is sent; a SHADOW guard only records a shadow hit and the ladder proceeds as on main (tests TestLadderGuards/ActiveResolveSystemicAtRung0, /ShadowRecordsOnly).
- [ ] 2. AC2 an active route guard makes the next rung route_specialist(profile) (test /ActiveRouteSkipsAskSource).
- [ ] 3. AC3 a valid accept_known + generalize=true + scope=lane human reply creates exactly one SHADOW suppress guard expiring at expires_in; generalize=false creates none (test /HumanGeneralizeCompilesShadow).
- [ ] 4. AC4 SweepGuards runs from the hourly tick; a demotion sends exactly one message to creator + challenger across two ticks; no guard can relax an identity/self-grading check (tests /DemotionNotifiesOnce, /NeverRelaxesIdentityGuard).
- [ ] 5. AC5 go vet ./... clean; go test -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/db/{exception_ladder.go (tx-level advance/resolve), guards.go (evaluator by point, human open compile anchored on an instance, Lane flag), guards_ladder.go (new: MatchPrecedent, routeSnapshotTx, ResolveHumanAtRung, GuardDemotionsSince)}, internal/relay/{exception_ladder.go, cleanup.go, guards_ladder_test.go (new)}
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -tags fts5 -race ./internal/db/... ./internal/relay/... OK

Files: 6 (ticket named 3 relay files; db additions flagged to wraith-cto in msg 9fad779e before coding: S1 had no ladder-point evaluator and no tx-level rung transitions).
Checklist: no schema/DDL, no tool-schema change. Guard hits, resolve/route and compile share the rung transition's single writer tx, which keeps the CAS on exceptions.rung (a lost race rolls back the hits). Messages go out after commit only (demotion notices from an audit-log cursor, so inline and sweep demotions each notify once). The sweep is gated to 1/h by a settings cursor (one write per hour, not per tick). Ladder-only paths run only in class_budget_mode=on, so this lands dark in prod (mode=shadow). protectedTx still excludes identity/auth/self_grading/integrity systemics from evaluation and from compile (test NeverRelaxesIdentityGuard). The suppress 2x-intensity backstop is unchanged and still tested (internal/db TestGuards/SuppressMassRegressionBackstop).

BLOCKERS: none
NITS:
- The design §6 7-day pre-expiry fyi is not in any AC and is not implemented; flagged for a follow-up.
- On its first run the demotion cursor starts empty, so any guard_demote audit events already in prod are announced once.

ROOT_CAUSE: feature (S2b), no defect. S1 left the ladder point, the human generalize compile and the lifecycle tick unwired.
DECISION: MatchPrecedent evaluates, then resolves/routes/advances in one tx. A route guard sets params.profile on the next route_specialist rung, inserting one after the current rung when none is ahead, and taskTarget honours that profile. accept_known generalize anchors the open suppress guard on the systemic's newest top-raiser instance (reading A, msg 9fad779e). Lane scope adds raised_by_profile only when the anchor has one. A refused human compile goes to a SAVEPOINT rollback and is recorded as generalize_error, so the human decision is never lost.
REJECTED: sending demotion notices only from SweepGuards' return (misses inline demotions); compiling before resolving (the origin must be resolved); failing the whole human resolution on a compile refusal.

RED_EVIDENCE:
  cmd: go test -tags fts5 -count=1 -run TestLadderGuards ./internal/relay/   (on a7efb39: test only, impl files at origin/main)
  test_sha: a7efb39
  output: |
    # agent-relay/internal/relay [agent-relay/internal/relay.test]
    internal/relay/guards_ladder_test.go:168:3: undefined: evaluateGuards
    internal/relay/guards_ladder_test.go:169:3: undefined: evaluateGuards
    internal/relay/guards_ladder_test.go:169:41: undefined: GuardSweepInterval
    FAIL	agent-relay/internal/relay [build failed]

## 3. Files changed

```
...ch-precedent-evaluates-guards-human-generali.md |  66 +++++++
 ...d-ladder-match-precedent-evaluates-guards-hu.md |  67 +++++++
 internal/db/exception_ladder.go                    |  50 +++--
 internal/db/guards.go                              |  65 +++++--
 internal/db/guards_ladder.go                       | 192 +++++++++++++++++++
 internal/relay/cleanup.go                          |  53 ++++++
 internal/relay/exception_ladder.go                 |  53 +++++-
 internal/relay/guards_ladder_test.go               | 204 +++++++++++++++++++++
 8 files changed, 709 insertions(+), 41 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-d17673fc-fe90-411e-ae3b-02e9a5b387f8
- 🟢 AC1: ActiveResolveSystemicAtRung0 asserts status=resolved, resolved_by=precedent, 0 messages with subject like %is it fixed?%, live_hits=1. ShadowRecordsOnly asserts rung advances to consult_knowledge, 1 shadow guard_hit, shadow_hits=1. Both run via go test -tags fts5 -race in doer worktree and pass. — evidence: internal/db/guards_ladder.go MatchPrecedent (commit 059341c): on GuardActionResolveSystemic calls resolveAtRungTx with resolvedBy=precedent and Commit, never reaches ask_source rung; on shadow, evaluateGuardsTx increments shadow_hits and returns nil guard so advanceRungTx runs as on main — test: TestLadderGuards/ActiveResolveSystemicAtRung0 and TestLadderGuards/ShadowRecordsOnly in internal/relay/guards_ladder_test.go (both PASS in doer worktree)
- 🟢 AC2: Test asserts rung=route_specialist after one tick, 2 inactive obligations on consult_knowledge+ask_source with discharge_evidence like %guard_route%, route_specialist task profile=spec-lane, 0 ask_source messages. PASS at run time. — evidence: internal/db/guards_ladder.go MatchPrecedent + routeSnapshotTx (commit 059341c): on GuardActionRoute calls routeSnapshotTx to set params.profile on the next route_specialist rung (inserting one when none ahead) then advanceRungTx to that index — test: TestLadderGuards/ActiveRouteSkipsAskSource in internal/relay/guards_ladder_test.go (PASS in doer worktree)
- 🟢 AC3: Test iterates generalize=true|false. For false: COUNT(compiled_guards)=0. For true: exactly 1 row with point=GuardPointOpen, action=GuardActionSuppress, mode=GuardModeShadow, source=GuardSourceHuman, created_by=human, from_exception_id=sys, scope contains class_id+project+raised_by_profile=dev, expires_at ~30d away. All sub-assertions pass. — evidence: internal/relay/exception_ladder.go generalizeCompile (commit 059341c): for accept_known + generalize=true sets gc.Point=GuardPointOpen, gc.Action=GuardActionSuppress, gc.ExpiresIn=h.expires; for generalize=false returns nil; passed to ResolveHumanAtRung which CompileResolutionTx commits with GuardSourceHuman — test: TestLadderGuards/HumanGeneralizeCompilesShadow in internal/relay/guards_ladder_test.go (PASS in doer worktree)
- 🟢 AC4: DemotionNotifiesOnce runs evaluateGuards twice, asserts guard demoted to shadow, exactly 1 message with subject 'guard % demoted to shadow', deliveries to creator cmo AND challenger rev. NeverRelaxesIdentityGuard sets kind=identity and asserts rung=consult_knowledge (no guard hit), guard_hits=0 for the systemic, and a follow-up compile returns GuardErrProtectedClass. Both pass. — evidence: internal/relay/cleanup.go evaluateGuards (commit 059341c): called from the ACK ticker loop; rate-limited to GuardSweepInterval (1h) via setting guard_sweep_at; after SweepGuards reads audit_log where action=guard_demote since setting guard_demote_notified_at and inserts one message per demotion, advancing the cursor. internal/db/guards.go protectedTx guards identity/auth/self_grading/integrity kinds via GuardErrProtectedClass in CompileResolutionTx and protectedTx short-circuit in evaluateGuardsTx — test: TestLadderGuards/DemotionNotifiesOnce and TestLadderGuards/NeverRelaxesIdentityGuard in internal/relay/guards_ladder_test.go (both PASS in doer worktree)
- 🔴 AC5: The deterministic gate exit=1 forces a reject per the brief (a FAIL forces a reject regardless of your per-criterion read). The code itself is sound in the doer worktree, but the review worktree is empty — a setup failure, not a code failure. Constraint says READ-ONLY; cannot populate the worktree. Verdict: reject. — evidence: Review worktree at /Users/loic/.agentd/qa/revwt-64710-d17673fc-fe90-411e-ae3b-02e9a5b387f8 contains ONLY .claude/settings.local.json — no internal/db, no internal/relay, no go.mod. git worktree list --porcelain marks this worktree prunable at HEAD 6ce678c. The verify command `go test -tags fts5 -race ./internal/db/... ./internal/relay/...` cannot chdir into internal/db. Daemon gate log: chdir .../revwt-64710-.../internal/db: no such file or directory. Same verify command run in the doer worktree (/Users/loic/Projects/agent-relay/.worktrees/wraith-engine-8110485e) reports 1213 PASS, go vet ./... clean, all 7 TestLadderGuards subtests PASS — but instructions forbid building there and the gate has already declared FAIL. — test: Verify command (go test -tags fts5 -race ./internal/db/... ./internal/relay/...) cannot execute in the review worktree; build fails with no such file or directory.

## 5. Timeline

- round 1 → **reject** (review-d17673fc-fe90-411e-ae3b-02e9a5b387f8)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `d17673fc-fe90-411e-ae3b-02e9a5b387f8`._
