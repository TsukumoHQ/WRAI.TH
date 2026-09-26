# [relay/knowledge] follow-up: change_class on remember + knowledge_rev in session_context

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/knowledge-followup (from main)
## Relay task : 298ae9b3-1bf4-4e0b-a5ac-f7204a40c7e7
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 remember(change_class=breaking) stores declared_class=breaking on its knowledge_log row; an invalid value -> INVALID_ARGUMENT, 0 rows; omitting it keeps today's default (undeclared HIGH = breaking). Test TestRememberChangeClass.
- [ ] 2. AC2 get_session_context returns knowledge_rev == max(rev) at boot; the boot adds no knowledge_log/consumption write beyond what consumption T1 already does. Test TestSessionContextCarriesKnowledgeRev.
- [ ] 3. AC3 TestToolSchemaBudget green with bytes stated; existing remember/decision tests unchanged.

## 2. Root cause & decisions

ROOT_CAUSE: knowledge S2 was held at 5 files. So remember() could not declare a change class (RememberDecision had no opts; decisions are always logged with the undeclared floor), and agents had no starting cursor for knowledge_delta at boot.

DECISION:
- RememberDecision takes variadic `opts ...SetMemoryOpts`, so the 7 existing callers compile unchanged. An invalid class is ErrInvalidChangeClass before any write, and the handler maps it to INVALID_ARGUMENT.
- remember gets `change_class` as a bare enum with no description (+88 B), the same values as set_memory.
- buildSessionContext adds `knowledge_rev` from db.KnowledgeDelta(project, MaxInt64): two RO reads, no write, in both the full and minimal shapes.

REJECTED: a separate head-rev db accessor. It would be a 6th file, and KnowledgeDelta at MaxInt64 already returns only the head and the watermark.

SCHEMA: +88 B. Margin is 2091 on ef72aba alone and ~4.0 KB once reclaim edad85b0 is merged (this branch is rebased on it before submit).

## review-wraith verdict: SHIP
Scope: internal/db/decisions.go, internal/relay/{tools.go,handlers.go,handlers_memory.go,knowledge_followup_test.go}
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... OK

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- The boot read of knowledge_rev is two cheap RO queries on every session_context; there is no write on the boot path (TestSessionContextCarriesKnowledgeRev asserts the head is unchanged).

## 3. Files changed

```
internal/db/decisions.go                  | 14 +++++-
 internal/relay/handlers.go                |  7 +++
 internal/relay/handlers_memory.go         |  6 ++-
 internal/relay/knowledge_followup_test.go | 73 +++++++++++++++++++++++++++++++
 internal/relay/tools.go                   |  1 +
 5 files changed, 98 insertions(+), 3 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `298ae9b3-1bf4-4e0b-a5ac-f7204a40c7e7`._
