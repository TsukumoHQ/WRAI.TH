# [relay/obligations] answer breach re-checks the reply, counting a reply that cites the ask id without reply_to

## Team : wraith-backend (tsukumo)
## Branch : fix/answer-cite (from main)
## Relay task : fc67d019-e419-48df-950f-427e5b172e25
## Trace : trace=72ea1558ecf4275c3e848df1fa65cdfd
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 ask A from X to Y; Y sends X a message with no reply_to whose subject contains A's 8-hex prefix; the breach sweep at deadline fulfils the answer.reply obligation and opens no answer.role row (test CitedReplyWithoutReplyToFulfilsAtBreach).
- [ ] 2. AC2 same cite but sent by a third agent, or by Y to someone other than X, does not fulfil: the rung escalates as on main (test CiteFromWrongPartyStillEscalates).
- [ ] 3. AC3 a reply_to reply that landed but whose fulfil was missed is fulfilled at breach instead of escalating (test BreachRechecksReplyToAnswer).
- [ ] 4. AC4 existing obligations answer tests pass unchanged; replay on a read-only COPY of the newest ~/.agent-relay/backups db lists in the PR body which escalated answer.* obligations would have been fulfilled (d2a49d2b expected among them).
- [ ] 5. AC5 go vet ./... clean; go test -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

# [relay/obligations] answer breach re-checks the reply, counting a reply that cites the ask id without reply_to

Task: fc67d019-e419-48df-950f-427e5b172e25

ROOT_CAUSE: `answeredBy` (internal/db/obligations.go) only followed reply_to chains, and `BreachAnswerObligation` escalated without re-checking message_answered at breach time. An answer that cites the ask id in its subject/content but omits reply_to (niwa-cto's 9701b420 "re 92a153b6: ..." at 09:49Z) never fulfilled answer.reply efffacbd, so the 10:51Z sweep opened answer.role d2a49d2b on cto-tsukumo.

DECISION (db layer only; no handler, schema or tool-schema change):
- `answeredBy(q, project, from[], messageID, since)` is now a free function over an `answerQ` read surface (the RO pool or the breach's writer tx). It returns `(replyID, match, ok)`, where match is `reply_to` (the existing chain walk) or `id_cite`. id_cite means a message from one of `from`, created at/after `since`, addressed to the ask's sender (`to_agent`, or a delivery row to the sender), whose lowercased subject or content contains the ask id's first 8 hex chars. The sender comes from messages, with message_tombstones as fallback. The ask itself is excluded.
- `answerChain` takes the same `answerQ`, so the whole predicate runs inside the breach tx. FulfilAnswerObligations keeps using `d.ro()`, so the send path is unchanged.
- `BreachAnswerObligation` re-checks the predicate inside its writer tx before the CAS breach. Answerers are the bearer plus the ask's original recipient (for a role rung), mirroring FulfilAnswerObligations, where a late answer from the recipient ends the chain it started. `since` is the ask's created_at. If the predicate holds, the obligation is fulfilled with CAS `WHERE state = active`, discharge_evidence `{"reply","match","by":"breach-recheck"}`, and the call returns `(nil, false, nil)`: no breach, no child, no sanction. The caller (cleanup.go) already skips on `!ok`.
- `DischargeAnswerObligation` uses the same predicate, so an id cite also satisfies an explicit discharge. Its evidence now carries `match`.

Lock discipline: the re-check reads run inside the single writer tx, only in the sweeper and only per due obligation (a rare path). The cite query plan is `SEARCH messages USING INDEX idx_messages_from`, with deliveries checked by idx_deliveries_message. On the prod backup the busiest sender has 690 rows and a cold query takes ~57ms. No hot-path write is added.

Verification: `go vet ./...` clean, `gofmt -l internal/` clean, `go test -tags fts5 -race -count=1 ./internal/db/... ./internal/relay/...` green (1185 tests).
- AC1: TestCitedReplyWithoutReplyToFulfilsAtBreach. A subject cite fulfils at breach with evidence match=id_cite naming the reply: no answer.role row, no relay message. A content cite works the same. The send path does not fulfil on a cite (unchanged).
- AC2: TestCiteFromWrongPartyStillEscalates. Three cases still escalate exactly as on main (rec unfulfilled, mgr active, 1 sanction): a third agent citing to the asker, the bearer citing to someone else, and the asker citing its own ask.
- AC3: TestBreachRechecksReplyToAnswer. A reply_to reply inserted past the send path (fulfil missed), reaching the ask through a 2-hop chain, is fulfilled at breach with match=reply_to.
- Extra: TestRoleRungRecheckCountsRecipientCite. After escalation, the original recipient's cite fulfils the role rung at its deadline, and no human rung opens.
- Revert check: with origin/main's obligations.go, AC1, AC3 and the extra test go red. AC2 is a negative test and is green on both.
- AC4: existing answer/obligation tests pass unchanged. Replay ran on a read-only copy (`mode=ro&immutable=1`) of the newest backup, `~/.agent-relay/backups/relay.20260926T113801Z.pre-aa57add.db`, with the new `answeredBy` over all 15 escalated answer.* obligations (state unfulfilled, no decline). **Would have been fulfilled at breach (answer landed before the breach):**
  - efffacbd answer.reply, ask 92a153b6 (backend-lead -> niwa-cto): reply 9701b420, id_cite, 09:49:35Z < breach 10:51:11Z. Its child is **d2a49d2b** (answer.role on cto-tsukumo), which would never have opened.
  - 1dba3a3f answer.reply, ask cfa918df (backend-lead-2 -> niwa-cto): reply 56df1fb6, id_cite, 10:07:48Z < breach 11:15:06Z. Its child 282c7770 (answer.role on cto-tsukumo, still active) would never have opened.
  - Answered only after the breach, so correctly escalated: 26a11072 (ask 3780a5ac, id_cite 33 min late) and 9e399e3d (ask b7c3d538, reply_to).

Rejected alternatives:
- Fulfilling on cite in the send path (FulfilAnswerObligations): that is a handler-side hook and outside the "db layer only" constraint. The breach re-check alone meets the goal, because an answered ask never escalates.
- A regex or "re <id>" grammar for cites: the task specifies a deterministic substring on the 8-hex prefix. Direction (bearer -> asker) plus the time window bounds collisions (32-bit prefix).

## review-wraith verdict: SHIP
Scope: internal/db/obligations.go (answerQ, answerChain, answeredBy, DischargeAnswerObligation, BreachAnswerObligation) + internal/relay/obligations_answer_cite_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 -race OK (internal/db + internal/relay, 1185 tests)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

Invariants checked: single writer (re-check plus fulfil in one writerTx with CAS on state; no second writer, no hot-path write). No schema or migration. agentColumns/scanAgent untouched. No MCP tool or schema change. Messages and deliveries are never modified by the breach path.

RED_EVIDENCE:
  cmd: go test -tags fts5 -race -count=1 -run 'Cite|BreachRechecks|RoleRungRecheck' ./internal/relay/   (on bb050c6 with internal/db/obligations.go reverted to origin/main)
  test_sha: bb050c6
  output: |
    --- FAIL: TestCitedReplyWithoutReplyToFulfilsAtBreach (0.04s)
        obligations_answer_cite_test.go:59: after the breach sweep: map[mgr:active rec:unfulfilled], want rec fulfilled only
    --- FAIL: TestBreachRechecksReplyToAnswer (0.03s)
        obligations_answer_cite_test.go:125: after the breach sweep: map[mgr:active rec:unfulfilled], want rec fulfilled only
    --- FAIL: TestRoleRungRecheckCountsRecipientCite (0.04s)
        obligations_answer_cite_test.go:145: after the role rung's deadline: map[mgr:unfulfilled rec:unfulfilled user:active], want rec unfulfilled, mgr fulfilled, no human rung
    FAIL
    FAIL	agent-relay/internal/relay	0.640s

## 3. Files changed

```
...hecks-the-reply-counting-a-reply-that-cites-.md |  90 +++++++++++++
 internal/db/obligations.go                         | 117 +++++++++++++---
 internal/relay/obligations_answer_cite_test.go     | 150 +++++++++++++++++++++
 3 files changed, 336 insertions(+), 21 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by human:wraith-cto

## 5. Timeline

- round 1 → **reject** (human:wraith-cto)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `fc67d019-e419-48df-950f-427e5b172e25`._
