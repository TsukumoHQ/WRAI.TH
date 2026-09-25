# [relay/norms] answer obligation on ask/decide, slice A: create + fulfil by reply + decline

## Team : wraith-engine (tsukumo)
## Branch : wraith/engine-044a4876 (from main)
## Relay task : 044a4876-98b5-4990-8ce1-d82ef6f1883b
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 ask: send_message action_required=ask to 2 direct recipients creates exactly 2 active answer.reply obligations (subject_kind=message, one per recipient); a reply with reply_to=that message from recipient A fulfils A's only; B's stays active. Test TestAnswerObligationAskFulfilledByReply.
- [ ] 2. AC2 decide + decline + tombstone: action_required=decide creates 1 active obligation; obligation_decline(id, reason_class=not_mine) on it closes it with decline_reason_class stored (no 'unknown obligation'); a second decide whose reply chain resolves through message_tombstones.reply_to is fulfilled. Test TestAnswerObligationDecideDeclineAndTombstone.
- [ ] 3. AC3 no obligation: messages with action_required none/ack/fyi, and an ask broadcast to '*', create 0 answer obligations; ack.* obligations for a task in the same test are byte-identical with and without the change. Test TestAnswerObligationNoneForNonAskAndBroadcast.

## 2. Root cause & decisions

ROOT_CAUSE: an ask/decide message had no obligation behind it. Nothing counted an unanswered question, and it died silently at TTL (prod: 36 asks, only 5 with reply_to). The obligations engine existed only for tasks (ACK chain).
DECISION (slice A of roadmap item 10; engine proposal 2d1cfba6, accepted with rulings 3836b334): a new norm family answer.* with subject_kind = 'message'. Every query and write is in separate functions keyed on that subject kind, so ack.* code paths are untouched.

DESIGN DELTAS
- Norm rows (seeded next to ack.*, INSERT OR IGNORE; no backfill, so only messages sent after deploy open obligations):
  answer.reply: depth 0, bearer_kind recipient, setting answer_reply_age default 3600s clamp 60..86400, on_unfulfilled answer.role
  answer.role: depth 1, bearer_kind recipient_role, setting answer_role_age default 7200s clamp 60..172800, on_unfulfilled answer.human
  answer.human: depth 2 = max_depth, bearer_kind human, no deadline, sanction message
  Closed enums: trigger message_ask, what message_answered, while message_open.
- Opened by: send_message on the DIRECT and TEAM paths, after the durable insert and not on an idempotent dedup hit, when the effective action_required is ask or decide. One obligation per resolved delivery recipient; sentinel principals (user/cron/linear) never bear one. Broadcast '*' and conversation messages open none. deadline_at = created + answer_reply_age.
- Fulfilled by: a message from the bearer whose reply_to chain reaches the ask. The walk reads messages.reply_to, then message_tombstones.reply_to for a purged link, max 8 hops, cycle-safe. The candidate read runs on the RO pool, so an ordinary reply opens no writer tx. Any reply (including broadcast/conversation) can fulfil.
- obligation_discharge: a message obligation falls back to DischargeAnswerObligation. The relay re-checks that the bearer sent a reply reaching the message since the obligation opened; the claim alone is never trusted.
- obligation_decline(id, reason_class): resolves message obligations too. Bearer only, active -> unfulfilled with decline_reason_class stored. No escalation yet (slice B).
- obligations_mine now also lists the caller's active answer obligations.
- Role resolution (used by slice B): the recipient's reports_to when active, else the first active executive that is not the recipient, else the human (max depth only).
- AC1 note: send_message takes one `to`, so "2 direct recipients" on one message is exercised via a two-member team ask (one message, two recipients); a direct ask is also asserted to open exactly one.

EVIDENCE (read-only copy of ~/.agent-relay/backups/relay.20260925T205333Z.pre-cef0f87.db in the session scratchpad, deleted after; newest message 2026-09-25T20:52Z; last 7 days):
ask direct 17, decide direct 30; team/broadcast/conversation asks 0 -> 47 messages, 47 expected obligations (~7/day).
20 of the 47 got a reply whose reply_to points at them, so ~27/week would breach once slice B escalates.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/db.go (3 norm seeds), internal/db/obligations.go (answer.* functions), internal/relay/handlers_messaging.go (trackAnswerObligations after the direct/team insert), internal/relay/handlers_obligations.go (mine/discharge/decline route message obligations), internal/relay/obligations_answer_test.go (3 AC tests), internal/db/obligations_test.go (norm-count assertion scoped to ack.*).
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -tags fts5 -race ./... OK (all packages; TestACKEquivalence and the ACK chain tests unchanged and green). On main the new tests do not compile (db.NormAnswerReply / db.SubjectMessage undefined).
Checks: single writer kept. Obligations open in their own short writer tx after the message commit, only for ask/decide. Fulfil writes only when an active obligation exists. The inbox is non-destructive: the message and its deliveries are never touched. A failure is logged and never fails the send. No schema change (norm rows only), no column-list change. Idempotent: UNIQUE(norm_id, bindings_hash) per message+recipient.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/obligations_test.go is a 6th file: its seed-idempotence check counted every norm row (4) and now counts ack.* (4) and answer.* (3) separately.
- The REST send path (ServeAPI) does not open answer obligations; the ticket scoped the MCP send_message handler. Follow-up if agents ask over REST.

## 3. Files changed

```
internal/db/db.go                         |  18 ++
 internal/db/obligations.go                | 296 ++++++++++++++++++++++++++++++
 internal/db/obligations_test.go           |   7 +-
 internal/relay/handlers_messaging.go      |  27 +++
 internal/relay/handlers_obligations.go    |  40 +++-
 internal/relay/obligations_answer_test.go | 227 +++++++++++++++++++++++
 6 files changed, 612 insertions(+), 3 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `044a4876-98b5-4990-8ce1-d82ef6f1883b`._
