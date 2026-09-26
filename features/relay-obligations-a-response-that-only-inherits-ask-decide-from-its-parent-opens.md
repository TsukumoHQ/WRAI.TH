# [relay/obligations] a response that only inherits ask/decide from its parent opens no answer obligation

## Team : wraith-engine (tsukumo)
## Branch : fix/response-inherited-ask-no-obligation (from main)
## Relay task : 9dacb162-1e8f-4d26-8c4b-05e5df2a4a6c
## Trace : trace=e7be5bc094fb7105a8aa515a95edfcec
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 send_message type=response, reply_to = an ask message, no action_required arg: 0 obligations rows opened and the stored action_required equals main's (test ResponseInheritedAskOpensNoObligation).
- [ ] 2. AC2 send_message type=response with explicit action_required=ask opens one answer.reply per recipient, as on main (test ResponseExplicitAskStillOpens).
- [ ] 3. AC3 type=question sends and explicit ask/decide on other types open obligations exactly as main: existing obligations_answer_test.go and obligations_answer_escalation_test.go pass unchanged.
- [ ] 4. AC4 PR body: on a read-only COPY of the newest ~/.agent-relay/backups db, count of answer.* obligations whose subject message is type=response with a derived tag (query stated) — the number this fix would have prevented.
- [ ] 5. AC5 go vet ./... clean; go test -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/relay/handlers_messaging.go (send_message answer-obligation open flag), internal/relay/obligations_answer_test.go (+2 tests)
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / go test -tags fts5 -race ./internal/db/... ./internal/relay/... OK

Change: answerable := action_required declared || type in {question,user_question}; both trackAnswerObligations call sites AND it into `open`. Fulfil path untouched (runs before the open gate). Stored action_required unchanged (deriveActionRequired untouched; AC1 test asserts inherited 'ask' is still stored). No schema, no tool-schema change, no new write.
Only type=response can derive ask/decide (task/policy/default -> do, reports -> none, question -> ask kept via type), so no other send changes behaviour.

Repro: TestResponseInheritedAskOpensNoObligation fails on origin/main handler: "inherited ask opened map[asker:active], want none".

AC4 (read-only copy of ~/.agent-relay/backups/relay.20260926T113801Z.pre-aa57add.db, opened file:...?immutable=1):
  SELECT COUNT(*), SUM(o.state='active'), COUNT(DISTINCT m.id)
  FROM obligations o JOIN messages m ON m.id=o.subject_id AND o.subject_kind='message'
  LEFT JOIN messages p ON p.id=m.reply_to LEFT JOIN message_tombstones pt ON pt.id=m.reply_to
  WHERE o.norm_id LIKE 'answer.%' AND m.type='response' AND m.reply_to IS NOT NULL
    AND m.action_required = COALESCE(p.action_required, pt.action_required);
  => 9 obligations (3 still active) on 4 response messages, of 53 answer.* total.
  Upper bound: a response that explicitly declared the same tag as its parent is indistinguishable in stored data.

BLOCKERS: none
NITS: none

ROOT_CAUSE: internal/db/messages.go deriveActionRequired gives a type=response reply its parent's stored tag (so a reply to an ask is stored as ask, correctly, for wake), and send_message's trackAnswerObligations opened answer.reply on any stored ask|decide — conflating "wake-worthy" with "owes an answer".
DECISION: gate the open on intent known at the handler (declared action_required, or type question/user_question); keep the stored/derived tag and wake behaviour byte-identical.
REJECTED: (a) change deriveActionRequired to store 'do'/'none' for responses — alters stored tag + wake, violates the ticket constraint; (b) return an explicit/derived bit from internal/db — needless API change, the handler already holds the declared arg.

RED_EVIDENCE:
  cmd: go test -tags fts5 -run TestResponse ./internal/relay/   (test file of 6e0a1a8, handlers_messaging.go reverted to origin/main)
  test_sha: 6e0a1a8
  output: |
    [FAIL] TestResponseInheritedAskOpensNoObligation
       obligations_answer_test.go:251: inherited ask opened map[asker:active], want none

## 3. Files changed

```
...ly-inherits-ask-decide-from-its-parent-opens.md | 76 ++++++++++++++++++++++
 internal/relay/handlers_messaging.go               |  8 ++-
 internal/relay/obligations_answer_test.go          | 46 +++++++++++++
 3 files changed, 128 insertions(+), 2 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by human:wraith-cto

### Round 2 — ❌ REJECTED by human:wraith-cto

### Round 2 — ❌ REJECTED by human:wraith-cto

### Round 3 — ❌ REJECTED by human:cto-tsukumo

## 5. Timeline

- round 1 → **reject** (human:wraith-cto)
- round 2 → **reject** (human:wraith-cto)
- round 2 → **reject** (human:wraith-cto)
- round 3 → **reject** (human:cto-tsukumo)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `9dacb162-1e8f-4d26-8c4b-05e5df2a4a6c`._
