# [relay] pin SetTaskRun CAS guard through public API (review-5972890c follow-up)

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/run-zone-cas-nits (from main)
## Relay task : deebb85c-2706-4061-b99b-9b10d07938c1
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. New test races the public SetTaskRun with real concurrent calls and hits >=1 CodeRunStateConflict
- [ ] 2. Test fails when the production guard is reverted (verified locally)
- [ ] 3. CodeRunStateConflict message no longer overclaims run_state as the only possible cause
- [ ] 4. go build/vet/test -tags fts5 all green

## 2. Root cause & decisions

ROOT_CAUSE: review-5972890c (merged 9cc8d8a) flagged that the two CAS tests I shipped (TestSetTaskRunCASRefusesStaleWrite, TestSetTaskRunCASGuardSerializesConcurrentWriters) issue the guard clause `WHERE run_state IS ?` as raw SQL inside the test body rather than going through SetTaskRun itself. The reviewer proved this with a revert-experiment: removing the guard from SetTaskRun's production UPDATE, both tests still passed unmodified while a real 12-racer hammer through the PUBLIC SetTaskRun landed 720/720 calls as silent overwrites (zero CodeRunStateConflict). So a future regression that quietly drops the guard from SetTaskRun would go undetected by CI. Separately, a notice flagged that the CodeRunStateConflict message text ("run_state changed from ...") assumes the 0-RowsAffected case is always a run_state change, when it's equally true for a deleted/archived row in that same window.

DECISION: Added TestSetTaskRunPublicAPIMixedTargetHammerPinsGuard — races the PUBLIC SetTaskRun with real concurrent goroutines (no shared pre-read snapshot; each call does its own read+write), mixing two valid targets (gating/blocked) off a shared 'open' state across 5 rounds x 20 racers. I reproduced the reviewer's exact revert-experiment locally before restoring the fix: with the guard removed, this new test fails loudly (0 conflicts across all rounds); the two existing tests still pass either way (expected — they test the guard clause directly, which is fine as an additional layer, just not sufficient alone). Reworded the conflict message to not assume specifically "run_state changed" since a concurrent delete/archive produces the identical RowsAffected=0 signal — the guidance (re-fetch and retry) is correct either way, so the message now says so without overclaiming the cause.

REJECTED ALTERNATIVES: none — straight fix of the two flagged findings.

[LEGACY_OPPORTUNITY]: none.

## review-wraith verdict: SHIP
Scope: internal/db/runs.go (CodeRunStateConflict message wording), internal/db/runs_test.go (new public-API hammer test)
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (573 passed, 12 packages) — new hammer test stress-ran 10x with -race, zero flakes; regression-verified locally (fails with the guard reverted, passes with it restored).

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
internal/db/runs.go      |  6 ++++-
 internal/db/runs_test.go | 68 ++++++++++++++++++++++++++++++++++++++++++++++++
 2 files changed, 73 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `deebb85c-2706-4061-b99b-9b10d07938c1`._
