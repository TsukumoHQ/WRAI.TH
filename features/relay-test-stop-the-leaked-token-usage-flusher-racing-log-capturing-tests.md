# [relay/test] stop the leaked token-usage flusher racing log-capturing tests

## Team : wraith-backend (tsukumo)
## Branch : fix/token-flusher-leak (from main)
## Relay task : d230b752-c277-416f-8e29-545dc3ac3c8d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. go test -tags fts5 -race -count=5 ./internal/relay/... is green on the branch (it reproduces the DATA RACE on clean main per wraith-backend 2026-09-25).
- [ ] 2. The flushTokenUsage goroutine started by NewHandlers (internal/relay/handlers.go ~:200) exits before the test's DB is closed, via a stop path used in test teardown (no sleep-based waits).
- [ ] 3. No production behaviour change: the flusher still flushes its buffer on channel close (existing flush-on-close path kept).
- [ ] 4. go vet ./... clean.

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/relay/handlers.go (Handlers.Close, stopCh/flushDone, flusher stop case, sweeper stop), 6 test files (teardown wiring + WarnOnce key + new pinned test). Task d230b752.
Gate: build -tags fts5 OK / vet (tags + no tags) OK / gofmt OK / go test -tags fts5 -race -count=5 ./internal/relay/... OK twice (10 iterations, 0 DATA RACE); same command on clean origin/main reproduces 3x DATA RACE + FAIL.

ROOT_CAUSE: NewHandlers starts flushTokenUsage (and sweepRegisterLimiters) with no stop path; tests close their DB in t.Cleanup but the flusher lives on. On its next 5s tick it tries InsertTokenUsageBatch on the closed DB and log.Printf's "token usage flush error: sql: database is closed" into whatever writer log currently points at — a later test's captured bytes.Buffer (TestSettingsSecretHandling, TestSettingAccessorsClampDefaultWarnOnce) -> DATA RACE on the buffer.
SECOND CAUSE (surfaced by the -count=5 AC): TestSettingAccessorsClampDefaultWarnOnce asserted one WARN for a fixed key, but the dedupe set db.settingWarnedKeys is process-wide by design (projects.go:147), so runs 2..N saw 0 WARNs. Test-only fix: unique key per run.
DECISION: Handlers.Close() = close(stopCh) once + wait on flushDone. The flusher's stop case drains the already-queued records, flushes, returns. tokenCh is deliberately NOT closed (RecordTokens / legacy estimate senders would panic on a closed channel); a send after Close fills the 256 buffer then takes the existing non-blocking drop path. Existing flush-on-channel-close branch kept verbatim. Each test site registers t.Cleanup(h.Close) AFTER the DB cleanup, so LIFO stops the flusher first. No sleeps.
REJECTED: mutex around a global log writer / silencing the flush error (hides the leak; ticket says fix the leak); closing tokenCh in Close (panic risk for concurrent senders); wiring Close into Relay.Shutdown (would change prod shutdown ordering — ticket says no prod behaviour change; left as a follow-up).

AC1: -count=5 race suite green (twice).
AC2: flusher exits before DB close via t.Cleanup(h.Close), no sleep; TestHandlersCloseFlushesAndStops asserts flushDone closed when Close returns, 3 queued records (below the 50 batch, inside the 5s tick) flushed on stop, idempotent Close, RecordTokens after Close does not panic.
AC3: prod never calls Close; flush-on-close path unchanged; sweeper behaviour unchanged (10-min ticker vs 10-min Sleep, same cadence).
AC4: go vet ./... clean.

Thesis checks: no new DB writer (the flusher is still the one batched writer, now also flushing on stop); no schema change; no hot-path write; no inbox/delivery semantics touched.

BLOCKERS: none

NITS:
- follow-up: Relay.Shutdown could call Handlers.Close so up to 5s of batched token usage is flushed on a graceful stop instead of lost (prod behaviour change, out of scope here).

## 3. Files changed

```
internal/relay/agent_budget_test.go      | 39 +++++++++++++++++++++++++++++
 internal/relay/api_test.go               |  1 +
 internal/relay/cleanup_test.go           | 13 +++++++---
 internal/relay/federation_test.go        |  1 +
 internal/relay/handlers.go               | 43 ++++++++++++++++++++++++++++++--
 internal/relay/handlers_test.go          |  4 ++-
 internal/relay/session_decisions_test.go |  2 ++
 7 files changed, 96 insertions(+), 7 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `d230b752-c277-416f-8e29-545dc3ac3c8d`._
