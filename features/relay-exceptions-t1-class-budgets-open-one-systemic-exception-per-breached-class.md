# [relay/exceptions] T1: class budgets open one systemic exception per breached class, attributed to its doctrine owner (shadow)

## Team : wraith-engine (tsukumo)
## Branch : wraith/budgets-t1 (from main)
## Relay task : b05cce16-8f73-413f-9f41-9b5b1f83afbe
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 breach semantics: TestClassBudgets/BreachOpensOneSystemic (3 instances of (p, dead_lane) in 7d -> one open systemic, 3 instances with cause_id); /FourthInstanceLinksNotReopens; /ConcurrentTicksOneSystemic (partial unique index, no error surfaced); /RegressionOfSet (resolve then 3 new -> new systemic with regression_of).
- [ ] 2. AC2 exclusions + attribution: /BenignAndUnclassifiedNeverBreach (10 plan_change + 10 unclassified -> none); /BackfillBeforeEpochIgnored; /Attribution reproduces §3.2 (dispatcher 100% -> owner; linear dispatcher skipped -> lane lead; no dimension >= 0.6 -> project executive; inactive owner climbs reports_to; never the founder).
- [ ] 3. AC3 shadow + migration: /ShadowRunsNoLadder (mode shadow -> rung NULL, no obligation with subject_kind=exception, no message via recorder notifier); /MigrateIdempotent (second migrate no-op, edited class_budgets row survives).

## 2. Root cause & decisions

ROOT_CAUSE: recurring friction (dead lanes, gate exhaustion, limbo sweeps) escalated per instance or not at all: nothing counted an exception class over time, so a class hitting 5 lanes in 2 days produced 5 unrelated notices and no owner (design 1111292b §1, D-exceptions B4/C1).
DECISION: OTP-style rate budget per (project, reason_code), default 3 per 7 d; on breach ONE systemic exception (partial unique index on open systemic per class), instances linked via cause_id, owner attributed deterministically (assignee/profile/dispatcher share >= 0.6, else process -> executive; non-agent dispatchers skipped; inactive owner climbs reports_to; never founder). budget_epoch excludes backfill. Shadow by default: record + attribute only, no rung/obligation/message. Whole design schema in one migration; ladder norms inert.
REJECTED: budget per fingerprint class (text variants split one friction, rarely breach; 220f4f3d §5.1); inline breach detection in the producer tx (adds reads to every block/cancel tx; sweeper keeps producers unchanged); a trigger-based link (cannot carry window semantics).

## review-wraith verdict: SHIP
Scope: internal/db/class_budgets.go (new), internal/db/exceptions.go (1 call), internal/relay/cleanup.go (tick hook), tests class_budgets_test.go + cleanup_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- class_budget_mode / attribution_share / budget_epoch are read via GetSetting but not declared in settings_spec.go (not in this ticket's file list); a REST PUT may refuse them until they are added.
- In dead projects (no active agent, no executive) the owner resolves to none (replay: 11 of 13 historical breaches); T2 must route owner=none to the supervisor walk.
- Tick cost when quiet: 2 RO queries (link check + 7 d window scan over exceptions, ~45 rows/day), zero writes.

Replay (read-only COPY of relay.20260925T225329Z backup, 385 blocked_reason rows through the S1 classifier, one tick per simulated day, 90 d): 13 systemic (expected ~14):
06-27 trovex-growth/gate_pending, 06-30 trovex-growth/limbo_sweep, 07-21 niwa/niwa_merge_blocked, 07-31 default/misrouted, 08-02 niwa/niwa_gate_rounds_exhausted, 08-02 niwa/stale_no_output, 09-06 overnight-saas/limbo_sweep, 09-06 skills-registry-v2/dead_lane, 09-06 synergix-standard/limbo_sweep, 09-06 trovex/dead_lane, 09-06 wraith-demo/dead_lane, 09-25 tsukumo/gate_pending (owner niwa-cto, assignee->reports_to), 09-25 tsukumo/niwa_gate_rounds_exhausted (owner cto-tsukumo, dispatcher).

## 3. Files changed

```
internal/db/class_budgets.go      | 577 ++++++++++++++++++++++++++++++++++++++
 internal/db/class_budgets_test.go | 303 ++++++++++++++++++++
 internal/db/exceptions.go         |   2 +
 internal/relay/cleanup.go         |  23 +-
 internal/relay/cleanup_test.go    |  59 ++++
 5 files changed, 963 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `b05cce16-8f73-413f-9f41-9b5b1f83afbe`._
