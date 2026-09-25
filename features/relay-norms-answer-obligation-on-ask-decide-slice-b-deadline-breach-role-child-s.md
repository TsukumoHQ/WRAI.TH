# [relay/norms] answer obligation on ask/decide, slice B: deadline breach, role child, sanction

## Team : wraith-engine (tsukumo)
## Branch : wraith/engine-a01d0b87 (from main)
## Relay task : a01d0b87-47d4-42c7-b45f-e8cf2ca3f24f
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 breach to role: ask to recipient R (reports_to M) unanswered past answer_reply_age: next sweep sets R's obligation unfulfilled and opens exactly one answer.role on M with parent_obligation_id set + exactly one P1 message to M quoting the ask; a second sweep opens nothing new. Test TestAnswerBreachOpensRoleChildOnce.
- [ ] 2. AC2 human only at max depth: role child unanswered past answer_role_age opens answer.human (depth 2) and a message to user; no to=user message exists before that; the original ask is still in R's inbox (delivery state unchanged). Test TestAnswerHumanOnlyAtMaxDepth.
- [ ] 3. AC3 child discharge + ACK isolation + unanswered query: a reply from M to the ask chain fulfils the role child; ack.* obligations and messages for a task in the same test are identical with and without the change; a db query (internal/db/obligations.go, e.g. UnansweredAsks(project, since)) returns per recipient the count of answer.* obligations still active or unfulfilled in the window, and obligations_mine lists R's open answer obligation. Test TestAnswerRoleChildFulfilledAckUntouched.

## 2. Root cause & decisions

ROOT_CAUSE: slice A (038d089) opened answer.reply obligations but nothing acted on their deadline. An unanswered ask stayed 'active' until its message died at TTL, so no one who could act was ever told.
DECISION (slice B of roadmap item 10; engine proposal 2d1cfba6 + rulings 3836b334/a5ca8508): evaluateObligations gets a separate message branch, evaluateAnswerObligations, which runs first so an ACK early return never skips it. The ACK code stays task-only and unchanged.
- DueAnswerObligations (RO): active message obligations with deadline_at <= now, joined to the ask (messages, else message_tombstones) and to the parent rung for the original recipient.
- BreachAnswerObligation (one writer tx): CAS active -> unfulfilled, then INSERT OR IGNORE the norm's on_unfulfilled child (parent_obligation_id, depth + 1, deadline from the child's setting). The bindings hash is keyed on message + original recipient + depth, so a re-run or a race opens no second child and sends no second sanction.
- Role resolution: answer.reply breaches to answer.role on the recipient's reports_to when active, else the first active executive that is not the recipient (the existing resolveAckRung2). If that resolves to the founder, or to the recipient, the chain skips to answer.human: the human is only ever the max-depth rung (depth 2). answer.role breaches to answer.human. answer.human has no deadline, so the chain stops there.
- Sanction: one P1 message (action do) from relay to the new rung's bearer, quoting the ask (subject + first 300 chars, or a purge note). It is sent as a reply to the ask, so the bearer answering the notice fulfils the rung through the slice-A chain walk. Pushed live via the notifier. The original message and its deliveries are never touched.
- Late answer: a reply from the original recipient also fulfils the active rungs opened for its obligation (child and grandchild), so an answered ask never reaches the human.
- UnansweredAsks(project, since): per bearer, the count of answer obligations created in the window that are active or unfulfilled, most first. obligations_mine already lists answer obligations (slice A).

EVIDENCE (read-only copy of ~/.agent-relay/backups/relay.20260925T210218Z.pre-038d0895.db in the session scratchpad, deleted after; the live DB was never opened):
before: answer norms=0, answer obligations=0, messages to user=21
opened with this build (slice A migration seeds the 3 answer.* norms), then one evaluateAnswerObligations sweep at 2026-09-25T21:06:37Z
after: answer norms=3, answer obligations=0, messages to user=21, answer sanctions=0 -> a deploy sweep emits 0 to=user messages (no backfill).

## review-wraith verdict: SHIP
Scope: internal/relay/cleanup.go (answer branch + sanction), internal/db/obligations.go (due/breach/UnansweredAsks, late-answer rung close), internal/relay/obligations_answer_escalation_test.go (3 AC tests).
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -tags fts5 -race ./... OK (all packages; TestACKEquivalence, the ACK chain tests and the slice-A tests unchanged and green).
Checks: single writer kept. The due read is on the RO pool; the breach is one short writer tx per due obligation, only when one is due; no write per idle tick. CAS on the parent plus the UNIQUE child makes the sanction fire once. The inbox is non-destructive. No human before max depth. No schema change. The ACK path is byte-identical with answer traffic present (AC3 test).

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- The founder skip means an agent with no reports_to and no executive in its project escalates straight to the human after answer_reply_age (1h), not after 1h + answer_role_age. This follows the design's "skip to next rung".

## 3. Files changed

```
internal/db/obligations.go                         | 174 +++++++++++++++++-
 internal/relay/cleanup.go                          |  89 +++++++++
 .../relay/obligations_answer_escalation_test.go    | 200 +++++++++++++++++++++
 3 files changed, 460 insertions(+), 3 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `a01d0b87-47d4-42c7-b45f-e8cf2ca3f24f`._
