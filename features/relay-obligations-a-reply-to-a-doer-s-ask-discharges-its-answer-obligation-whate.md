# [relay/obligations] a reply to a doer's ask discharges its answer obligation whatever thread it lands on, so the lead is never escalated a stale question

## Team : wraith-engine (tsukumo)
## Branch : wraith/8f62ecb7-answer-offthread (from main)
## Relay task : 8f62ecb7-b46d-4b25-a131-cfd2a48af944
## Trace : trace=0a893ab6b7c2198a655d4bbffa7b434d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: ask A->B with task_id T, B sends A a new message with task_id T (no reply_to) -> obligation discharged, no escalation
- [ ] 2. test: no message from B to A -> escalation fires as today
- [ ] 3. test: the escalation body lists B's last message to A after the ask, if any

## 2. Root cause & decisions

# 8f62ecb7 — answer obligation discharges on a same-task message

ROOT_CAUSE: internal/db/obligations.go answeredBy (message_answered predicate, re-checked at breach in BreachAnswerObligation) matched only a reply_to chain or an 8-hex id cite. niwa-cto-2 answered doers in a NEW message tied to the same task (metadata task_id, no reply_to, no cite), so answer.reply breached and answer.role escalated a stale question to cto-tsukumo. The sanction (internal/relay/cleanup.go sendAnswerSanction) also gave the lead no hint that the recipient had already written the asker.

FIX: answeredBy gains a third match kind task_id: a message from the bearer (or root recipient) to the asker, after the ask, whose messages.task_id equals the ask's task_id (read from messages or message_tombstones). sendAnswerSanction appends "| <recipient> last wrote <asker> at <ts> (msg <id8>): <subject>" from new RO read DB.LastMessageTo. No schema change, no new write path.

RED_EVIDENCE:
  cmd: niwa slot run -- go test -tags fts5 ./internal/relay -run 'SameTask|SanctionQuotesBearer' -count=1
  test_sha: 4f42c3b
  output: --- FAIL: TestSameTaskMessageFulfilsAnswerAtBreach: after the breach sweep: map[mgr:active rec:unfulfilled], want rec fulfilled only
          --- FAIL: TestAnswerSanctionQuotesBearerLastMessage: sanction "Unanswered ask from asker to rec after 61min (escalated to you as rec's lead): \"s\": drop column x?" does not quote rec's last message a873bc25 'status update'
  (TestSameTaskMessageWrongPartyOrTaskStillEscalates = AC2 guard, green before and after)

## review-wraith verdict: SHIP
Scope: internal/db/obligations.go (answeredBy, LastMessageTo), internal/relay/cleanup.go (sendAnswerSanction), internal/relay/obligations_answer_task_test.go
Gate: vet -tags fts5 / gofmt / test -tags fts5 ./... (see done report)
BLOCKERS: none
NITS: a bearer's unrelated message on the same task (e.g. "starting") now also discharges the ask — intended per ticket WANT.
Migration note: none (messages.task_id and message_tombstones.task_id already exist).

## 3. Files changed

```
internal/db/obligations.go                     |  62 +++++++++----
 internal/relay/cleanup.go                      |  11 +++
 internal/relay/obligations_answer_task_test.go | 115 +++++++++++++++++++++++++
 3 files changed, 173 insertions(+), 15 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `8f62ecb7-b46d-4b25-a131-cfd2a48af944`._
