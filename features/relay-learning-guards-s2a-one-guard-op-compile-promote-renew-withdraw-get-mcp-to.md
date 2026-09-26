# [relay/learning] guards S2a: one guard(op=compile|promote|renew|withdraw|get) MCP tool over the S1 db layer

## Team : wraith-engine (tsukumo)
## Branch : feat/guard-tool (from main)
## Relay task : 1c56b1c3-bbb4-4e27-a57a-107730ca09fc
## Trace : trace=f0e44fecc48073b43ebf15b1796d7b69
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 guard(op=compile) by the origin's resolver returns the guard row plus replay counts (matched/agree/conflicts/undecided); by an unrelated agent it is refused with 0 rows written (tests TestGuardTool/CompileReturnsReplay, /CompileRefusedThirdParty).
- [ ] 2. AC2 guard(op=promote) by created_by or by the origin resolver is refused; by a third agent at 3 clean shadow hits it succeeds and sets challenged_by (test /PromoteRefusedSameAgent).
- [ ] 3. AC3 guard(op=renew) without live hits or by the creator is refused; guard(op=withdraw) moves the guard to retired and it no longer matches (tests /RenewNeedsLiveHitsAndOtherAgent, /WithdrawStopsMatching).
- [ ] 4. AC4 TestToolSchemaBudget green with constants unchanged; PR body states the guard tool's bytes (target <=500 B) and the remaining margin.
- [ ] 5. AC5 go vet ./... clean; go test -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/relay/{handlers_guards.go (new), tools.go, toolset.go, guards_test.go (new)}, internal/db/guards.go (WithdrawGuard + Guard JSON tags)
Gate: build -tags fts5 OK / vet (plain + fts5) OK / gofmt OK / go test -tags fts5 -race ./internal/db/... ./internal/relay/... OK (1184 passed)

AC4 schema budget: guard tool = 500 B. Total 55214 B of 57344 cap, margin 2130 B (headroom 2048 held; constants unchanged). Discovery pair 1809 B.
How 500 B: point derived from action (closed enum, each action valid at exactly one point); action enum enforced in HandleGuard, not the schema; annotations cleared on this tool only. The MCP spec defaults for absent hints (readOnly=false, destructive=true, idempotent=false, openWorld=true) equal mcp-go's explicit defaults, so clients read the same hints.
Category: memory (the tasks discovery payload hit its 16000 B cap with guard in it: TestDiscoverPayloadSize).

Checklist: no schema/DDL change; every write is one writer tx (db layer); WithdrawGuard uses a guarded UPDATE ... WHERE mode IN ('shadow','active') + RowsAffected (no TOCTOU); authority lives in db (compile: resolver/owner; promote: not creator/resolver, evidence; renew: not creator, live hit; withdraw: creator/challenger/resolver/owner); guard of another project -> GUARD_NOT_FOUND; tool behind guardIdentity like every mutating tool; guards never touch protected kinds (S1 GuardErrProtectedClass).

BLOCKERS: none
NITS:
- compile does not check the origin exception's project against the caller's project (S1 CompileResolution takes no project); authority is by resolver/owner name, and identity is enforced per project by guardIdentity.

ROOT_CAUSE: feature (S2a), no defect. The S1 db layer (7ccda50) had compile/promote/renew with no agent-facing surface and no withdraw.
DECISION: one guard(op=...) tool per ruling 3ece19c0 OQ5; point implied by action to save schema bytes; withdraw authority = creator + challenger + origin resolver + systemic owner (reading B, flagged to wraith-cto in relay msg d57c629d: withdrawal only retires, never relaxes).
REJECTED: four separate tools (schema cost ~4x); keeping point as a param (+26 B with no information, since the action determines it); raising the budget cap (ruling ed744dee forbids).

RED_EVIDENCE:
  cmd: go test -tags fts5 -count=1 -run TestGuardTool ./internal/relay/   (on 06b1cb9: test only, impl files at origin/main)
  test_sha: 06b1cb9
  output: |
    # agent-relay/internal/relay [agent-relay/internal/relay.test]
    internal/relay/guards_test.go:65:16: f.h.HandleGuard undefined (type *Handlers has no field or method HandleGuard)
    internal/relay/guards_test.go:75:16: f.h.HandleGuard undefined (type *Handlers has no field or method HandleGuard)
    internal/relay/guards_test.go:183:16: undefined: db.GuardErrWithdrawRefused
    FAIL	agent-relay/internal/relay [build failed]

## 3. Files changed

```
...op-compile-promote-renew-withdraw-get-mcp-to.md |  69 ++++++++
 internal/db/guards.go                              |  80 +++++++--
 internal/relay/guards_test.go                      | 185 +++++++++++++++++++++
 internal/relay/handlers_guards.go                  |  97 +++++++++++
 internal/relay/tools.go                            |  21 +++
 internal/relay/toolset.go                          |   3 +-
 6 files changed, 444 insertions(+), 11 deletions(-)
```

## 4. QA Log

### Round 3 — ❌ REJECTED by human:wraith-cto

### Round 4 — ❌ REJECTED by human:cto-tsukumo

## 5. Timeline

- round 3 → **reject** (human:wraith-cto)
- round 4 → **reject** (human:cto-tsukumo)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1c56b1c3-bbb4-4e27-a57a-107730ca09fc`._
