# [wraith/relay][split] lint hotfix: QF1001 limbo_sweep.go:149 + S1030 console_v2_config_panel_test.go:104 (main red, blocks all PR CI)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/lint-hotfix (from main)
## Relay task : 0644c8b8-dba5-46db-a880-c64593f3be09
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 golangci-lint run clean on the two flagged files (QF1001 + S1030 gone), no new findings introduced — lint output cited
- [ ] 2. AC2 go test -tags fts5 ./... green (count cited) — limbo sweep tests + console_v2 config panel tests unchanged and passing
- [ ] 3. AC3 diff touches exactly limbo_sweep.go (1 line region) + console_v2_config_panel_test.go (1 line); no other file

## 2. Root cause & decisions

# .niwa-decision — 0644c8b8 lint hotfix (main lint-green)

ROOT_CAUSE: main tip 63902c2 is RED under golangci-lint v2.12.2, surfaced by PR #167 — the first PR through the newly-installed org-wide niwa-gate PR CI path. Two pre-existing staticcheck findings (neither in the 96eb1fa3 diff, lead-verified on main): (1) internal/db/limbo_sweep.go:149 QF1001 (De Morgan's law applicable); (2) internal/relay/console_v2_config_panel_test.go:104 S1030 (should use w.Body.Bytes() not []byte(w.Body.String())). These block EVERY PR's CI fleet-wide until main is green.

DECISION (per lead ruling (a) on msg 48f73be5 — hotfix FIRST, no fold-in, keep 96eb1fa3 scope clean): exactly the 2 one-line fixes, zero behavior change.
- limbo_sweep.go:149: `if !(r.lastActivityAt < blockCut && r.agentLastSeen < blockCut) {` -> `if r.lastActivityAt >= blockCut || r.agentLastSeen >= blockCut {`. Body is `continue`; De Morgan-equivalent. Missing-timestamp rows already filtered by the `== ""` guard at :146, so both operands are pure string ordering — no nil/empty edge changes.
- console_v2_config_panel_test.go:104: `json.Unmarshal([]byte(w.Body.String()), &body)` -> `json.Unmarshal(w.Body.Bytes(), &body)`. Same bytes, one fewer copy.

REJECTED ALTERNATIVE: fold the fixes into 96eb1fa3 (option b) — rejected by lead; violates that ticket's ≤2-file / other-files-untouched constraint and mixes an unrelated concern into the orphan_profile change.

VERIFIED: golangci-lint v2.12.2 `run --timeout=5m ./...` = 0 issues; go build -tags fts5 ./... OK; go test -tags fts5 -count=1 ./... green. Sequence after merge: rebase wraith-backend/orphan-profile-redef onto new main, re-submit 96eb1fa3.

## review-wraith verdict: SHIP
Scope: internal/db/limbo_sweep.go (1 line) + internal/relay/console_v2_config_panel_test.go (1 line). No other file.
Gate: gofmt clean / golangci-lint 0 issues / go build -tags fts5 OK / go test -tags fts5 -count=1 ./... green.
Tests (existing suites, no additions): TestLimboSweepExemptsActiveServiceAndFreshClocks, TestLimboSweepBlocksInactiveAssigneeAndDispatcher, TestV2ConfigPanel.
BLOCKERS: none. NITS: none. Zero behavior change; pure lint conformance.

## 3. Files changed

```
internal/db/limbo_sweep.go                     | 2 +-
 internal/relay/console_v2_config_panel_test.go | 2 +-
 2 files changed, 2 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `0644c8b8-dba5-46db-a880-c64593f3be09`._
