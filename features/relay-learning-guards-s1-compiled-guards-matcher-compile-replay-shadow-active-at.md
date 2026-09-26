# [relay/learning] guards S1: compiled_guards matcher, compile + replay, shadow/active at exception open, lifecycle (db)

## Team : wraith-engine (tsukumo)
## Branch : wraith/guards-s1 (from main)
## Relay task : e0dc3ecf-ab8e-471e-949e-09bc9d953662
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 compile + replay + safety: TestGuards/UnknownScopeKeyRejected, /AnchorRequired, /ScopeMustMatchOrigin, /ExpiryRequiredAndCapped90d (refused, 0 rows); /ReplayClassifiesAgreeConflict (12 exceptions over 100d, 3 older ignored, matches checked-in expectation per action); /NoModelAtMatchTime (matcher deps stdlib + database/sql only via go list -deps; same hits on two fresh DBs); /NeverOverridesIdentityOrAuthGuards (a compiled guard scoped to an identity/self-grading exception class cannot suppress or resolve it; auth paths untouched).
- [ ] 2. AC2 shadow, promotion, budgets: /ShadowRecordsNeverActs (3 shadow hits, all 3 still count toward the class budget); /ActiveSuppressExcludesFromBudget (5 matching opens, live_hits=5, budget 3/7d not breached, non-matching still count); /PromotionNeedsDifferentAgentAndEvidence (creator, origin resolver, 2 hits, or 1 shadow conflict refused; third agent at 3 clean hits succeeds, challenged_by set); /PrecedenceMostSpecificActs; /SuppressMassRegressionBackstop (suppressed > 2x class intensity -> regression -> demote to shadow; cto enforce-flip gate depends on this).
- [ ] 3. AC3 lifecycle + atomicity: /RegressionDemotes (two recurrences after anchor+grace -> shadow + audit event; inside grace not counted); /ExpireAndRetire; /OpenTxAtomic (forced guard-hit insert failure rolls back the exception insert and its producer transition).

## 2. Root cause & decisions

ROOT_CAUSE: the same judgment was made again and again (57 recurring classes, 31 recur >= 5x on the prod copy): a resolution, human or agent, lived in prose with no scope, no expiry, no hit count and no link to the exception it settled, so nothing could apply it next time or notice when it stopped being true (design 4d2e57a3 §1).
DECISION: compiled_guards + guard_hits side tables; a closed JSON matcher (class_id / kind / reason_code / retry_class / project / source_kind / raised_by_profile / template_prefix, anchor required, must match its origin) evaluated in Go inside openExceptionTx (same writer tx; a failed hit write fails the producer). Born shadow; 90-day replay at compile; promotion only by an agent other than the creator and the origin's resolver after >= 3 clean shadow hits and 0 replay conflicts; active open-point suppress excludes the instance from class-budget counting and linking; mass backstop (> 2x class intensity per period) and anchored recurrences demote to shadow with an audit event; expiry mandatory <= 90d, never auto-renewed; retire after 30d without hits. Protected kinds (identity, self_grading, auth, integrity) are refused at compile and skipped at evaluation. Ruling 3ece19c0.
REJECTED: deny/transition enforcement point (ruled OQ1: bookkeeping only); auto-promotion (OQ3); a settings constants file (knobs read via SettingInt/SettingDuration); editing exception_ladder.go in S1 (systemic-resolve settling is caught by SweepGuards; the ladder point lands in S2).

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/guards.go (new), internal/db/exceptions.go (migrateGuards, evaluate at open, settle at resolve), internal/db/class_budgets.go (suppressed instances excluded from scan, instances and linking), internal/db/guards_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- NoModelAtMatchTime pins guards.go's imports (stdlib only) with go/parser rather than `go list -deps`: package db necessarily depends on the sqlite driver and uuid, so a package-level deps walk cannot express the rule.
- Demotion resets shadow_hits and shadow_conflicts to 0 (fresh clean evidence needed to re-promote) instead of setting shadow_conflicts = regressions as §6 words it, which would block re-promotion forever under §5.2's shadow_conflicts = 0 rule. History is in the audit log.
- Suppress mass regression demotes at once (the backstop the cto enforce-flip depends on) rather than waiting for guard_demote_regressions.
- Equal-specificity/different-action conflict is exercised with differing action_params: only suppress exists at the open point in S1.
- Ladder-point guards compile and replay here; their hits, the override regression and pre-expiry/demotion messages land with S2.

## 3. Files changed

```
internal/db/class_budgets.go |   14 +-
 internal/db/exceptions.go    |   20 +-
 internal/db/guards.go        | 1214 ++++++++++++++++++++++++++++++++++++++++++
 internal/db/guards_test.go   |  602 +++++++++++++++++++++
 4 files changed, 1844 insertions(+), 6 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `e0dc3ecf-ab8e-471e-949e-09bc9d953662`._
