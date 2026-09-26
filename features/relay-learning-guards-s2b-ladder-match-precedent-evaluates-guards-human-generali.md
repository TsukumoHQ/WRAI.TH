# [relay/learning] guards S2b: ladder match_precedent evaluates guards, human generalize compiles a shadow guard, hourly SweepGuards

## Team : wraith-engine (tsukumo)
## Branch : feat/guards-ladder (from main)
## Relay task : 8110485e-ad2b-490c-b09c-d8253c7b5b7c
## Trace : trace=51b6ca269f82b28cc6ef3274ae07b6c7
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 a matching ACTIVE ladder guard resolves the systemic at match_precedent with resolved_by=precedent and no ask_source message is sent; a SHADOW guard only records a shadow hit and the ladder proceeds as on main (tests TestLadderGuards/ActiveResolveSystemicAtRung0, /ShadowRecordsOnly).
- [ ] 2. AC2 an active route guard makes the next rung route_specialist(profile) (test /ActiveRouteSkipsAskSource).
- [ ] 3. AC3 a valid accept_known + generalize=true + scope=lane human reply creates exactly one SHADOW suppress guard scoped {class_id, project, raised_by_profile} expiring at expires_in; generalize=false creates none (test /HumanGeneralizeCompilesShadow).
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
  cmd: go test -tags fts5 -count=1 -run TestLadderGuards ./internal/relay/   (on f6ab2b1: test only, impl files at origin/main)
  test_sha: f6ab2b1
  output: |
    # agent-relay/internal/relay [agent-relay/internal/relay.test]
    internal/relay/guards_ladder_test.go:168:3: undefined: evaluateGuards
    internal/relay/guards_ladder_test.go:169:3: undefined: evaluateGuards
    internal/relay/guards_ladder_test.go:169:41: undefined: GuardSweepInterval
    FAIL	agent-relay/internal/relay [build failed]

## 3. Files changed

```
internal/db/exception_ladder.go      |  50 ++++++---
 internal/db/guards.go                |  65 +++++++----
 internal/db/guards_ladder.go         | 192 +++++++++++++++++++++++++++++++++
 internal/relay/cleanup.go            |  53 +++++++++
 internal/relay/exception_ladder.go   |  53 +++++++--
 internal/relay/guards_ladder_test.go | 204 +++++++++++++++++++++++++++++++++++
 6 files changed, 576 insertions(+), 41 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `8110485e-ad2b-490c-b09c-d8253c7b5b7c`._
