# [relay/budget] boot selection journal: log projectMessages selection in get_session_context

## Team : wraith-backend (tsukumo)
## Branch : feat/boot-selection-journal (from main)
## Relay task : 409fb2e8-313f-461a-80a4-54064de899d9
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. Every projectMessages call with >=1 candidate emits exactly one [budget] log line in the same shape as the applyBudget journal from task 1f190795 plus a field path=session_context and the agent name.
- [ ] 2. The line lists candidate ids, projected ids, omitted ids, per-message bytes, and budget used/max.
- [ ] 3. A pinned test in internal/relay asserts the fields of the line for a case where the budget omits at least one message.
- [ ] 4. get_session_context output is byte-identical before and after the change (test compares the projection for a fixed input).
- [ ] 5. go vet ./... clean; go test -tags fts5 -race ./internal/relay/... green.

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/relay/project.go (projectMessages journal), budget.go (shared journal struct), handlers.go:846 (pass agentName), project_test.go (3 call sites), budget_test.go, project_journal_test.go (new). Task 409fb2e8.
Gate: build -tags fts5 OK / vet (tags + no tags) OK / gofmt OK / go test -tags fts5 -race ./internal/relay/... OK (single run)

ROOT_CAUSE: the only budget journal (1f190795) sits in applyBudget, which never runs in prod; the live boot budget is projectMessages (6000 B, P0 first then created_at DESC) and left no selection record, so the quality/budget research had no data from the path that actually runs.
DECISION: reuse budgetJournal (one struct, one emit) — added path, agent, omitted; Score became *float64 omitempty because projectMessages ranks without a utility (no invented score). projectMessages gains an agent param (sole prod caller handlers.go:846 passes agentName) and emits one deferred [budget] line per call with >=1 candidate: candidates (input order), selected (== projected ids, in order), omitted (soft-budget and hard-ceiling drops), scores[{id,priority,bytes}] with bytes = messageSummaryBytes of the projected summary (what the budget counts), budget_used/max. applyBudget now also tags path=get_inbox and lists omitted ids (same shape both paths).
REJECTED: a second journal type for the boot path (task says reuse, don't duplicate); a wrapper keeping the 2-arg signature (would let a future caller skip the agent name); logging raw message bytes instead of projected-summary bytes (would not replay the budget arithmetic).

AC1: TestProjectMessages_JournalLine — exactly one line, path=session_context, agent=wraith-backend.
AC2: same test asserts candidates, selected, omitted (3 omitted), per-message bytes, budget used == sum(selected bytes)/max.
AC3: fixture budget 900 omits m-p2-old, m-p3, m-unknown.
AC4: TestProjectMessages_OutputByteIdenticalWithJournal — golden JSON captured from origin/main BEFORE the change, compared byte-for-byte.
AC5: vet clean; relay race suite green.

Thesis checks: no DB write (log.Printf only, one line per boot, ids/priorities/bytes — no content); projection output unchanged (golden); no schema change; no new dependency; sessionUnreadBudget untouched; applyBudget scoring untouched.

BLOCKERS: none

NITS:
- PRE-EXISTING FLAKE (not this diff, reproduced on clean origin/main with -count=3): TestSettingsSecretHandling / TestSettingAccessorsClampDefaultWarnOnce capture log output into a bytes.Buffer while a leaked flushTokenUsage goroutine (handlers.go:213, from an earlier test's NewHandlers) writes "token usage flush error: sql: database is closed" into it -> DATA RACE. Separate ticket: stop the flusher in test teardown or guard the capture buffer.

## 3. Files changed

```
internal/relay/budget.go               |  31 +++++---
 internal/relay/budget_test.go          |  19 +++--
 internal/relay/handlers.go             |   2 +-
 internal/relay/project.go              |  18 ++++-
 internal/relay/project_journal_test.go | 128 +++++++++++++++++++++++++++++++++
 internal/relay/project_test.go         |   6 +-
 6 files changed, 186 insertions(+), 18 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `409fb2e8-313f-461a-80a4-54064de899d9`._
