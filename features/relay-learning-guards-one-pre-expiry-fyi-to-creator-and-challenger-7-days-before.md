# [relay/learning] guards: one pre-expiry fyi to creator and challenger 7 days before expires_at

## Team : wraith-engine (tsukumo)
## Branch : feat/guards-preexpiry (from main)
## Relay task : 5b16f32d-70c1-4e42-a00c-096fb5ef368d
## Trace : trace=de7716e2472d216e032876126b20d13d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 a guard whose expires_at is within 7 days gets exactly one fyi to creator and challenger across any number of hourly ticks (test PreExpiryNotifiesOnce).
- [ ] 2. AC2 a guard renewed after the notice and later re-entering the 7-day window is notified again once (test PreExpiryRearmsAfterRenew).
- [ ] 3. AC3 retired, expired or withdrawn guards get no notice (test PreExpirySkipsClosed).
- [ ] 4. AC4 go vet clean; go test -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/guards.go (additive column, SweepGuards.Expiring, guardsDueExpiryNotice, MarkGuardExpiryNoticed), internal/relay/cleanup.go (evaluateGuards sends the fyi after the sweep commits; guardOwners extracted from the demotion loop and reused), internal/relay/guards_ladder_test.go (TestGuardPreExpiry).
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / go test -tags fts5 -race ./internal/db/... ./internal/relay/... 1224 passed

BLOCKERS: none

Checklist: single writer kept: the mark is one writer tx per noticed guard, run from the hourly sweep only, never on a request path. Schema is additive and idempotent: ensureColumns(compiled_guards, expiry_noticed_for TEXT) runs in migrateGuards right after CREATE TABLE, before any read; guardColumns/scanGuard untouched (the new column is read by its own query). An older binary ignores the column. Messages: type fyi, P2, action none (no wake), inserted after SweepGuards' txs commit, like the S2b demotion notice. No tool-schema change.
Notify once: the mark stores the expires_at that was announced; the due query skips a guard whose expiry_noticed_for equals its expires_at. A renewal moves expires_at, so the notice re-arms by construction. The mark is a CAS on expires_at, so a renewal racing the notice leaves the new expiry armed. Closed guards: the query is limited to mode shadow/active and expires_at > now, and runs after the sweep has ended the lapsed ones.
Mutation check: with the mark call removed, PreExpiryNotifiesOnce sees 3 notices over 3 ticks and PreExpiryRearmsAfterRenew sees 3, both FAIL.

NITS:
- If the message insert lands and the mark write then fails, the notice repeats on the next hourly sweep (logged). Same at-least-once shape as the demotion cursor.
- Shadow guards get the notice too (AC: active or shadow); they cannot be renewed, so the body says only an active guard can be renewed.

ROOT_CAUSE: guards expire at a mandatory <=90d with no auto-renew (ruling 3ece19c0), and nothing warned the owners before the expiry.
DECISION: SweepGuards returns the due guards and cleanup.evaluateGuards sends one fyi each and marks it against that expires_at.
REJECTED: an audit-log cursor like demotions (a cursor cannot re-arm per guard after a renewal); a boolean noticed flag (renew would have to clear it, a second write site in RenewGuard).

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/ -run TestGuardPreExpiry   (on 633bf2b: test file only)
  test_sha: 633bf2b
  output: |
    --- FAIL: TestGuardPreExpiry (0.17s)
        guards_ladder_test.go:233: pre-expiry notices = 0 across three ticks, want 1
        guards_ladder_test.go:257: notices after renewal = 0, want still 1
    FAIL
    FAIL	agent-relay/internal/relay	12.807s

## 3. Files changed

```
internal/db/guards.go                | 50 +++++++++++++++++++++-
 internal/relay/cleanup.go            | 51 ++++++++++++++++------
 internal/relay/guards_ladder_test.go | 82 ++++++++++++++++++++++++++++++++++++
 3 files changed, 170 insertions(+), 13 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `5b16f32d-70c1-4e42-a00c-096fb5ef368d`._
