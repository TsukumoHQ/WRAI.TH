# [relay/escalation] an unanswered 'decide' from A to B escalates to A itself as 'B's lead' (chain walks the wrong way)

## Team : wraith-engine (tsukumo)
## Branch : wraith/b62b3966-escalation-chain (from main)
## Relay task : b62b3966-7d67-40fa-9ad1-9375ad7ac74c
## Trace : trace=255eaeced52cae57e2f80646478eb60a
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. test: A reports_to B, B reports_to F; A's unanswered decide to B escalates to F
- [ ] 2. test: escalation target == sender is skipped (next up, or dropped with an audit line)
- [ ] 3. test: an inactive agent whose reports_to points at the sender does not make the sender the recipient's lead
- [ ] 4. root cause named file:line in the PR body
- [ ] 5. Go suite green with -tags fts5

## 2. Root cause & decisions

# b62b3966 escalation chain

ROOT_CAUSE: internal/relay/cleanup.go:651 (origin/main) resolved the answer.role bearer with resolveAckRung2(recipient), the ACK ladder resolver, which knows nothing about the asker. cto-tsukumo reports_to 'founder' (not a registered agent), so GetAgent missed and it fell to the "any active executive" fallback at cleanup.go:624, which only excludes the recipient; the first active executive was niwa-cto-2, the asker. Fix: resolveAnswerRung walks the recipient's reports_to chain, skips the asker and inactive agents one rung up, treats a non-agent reports_to as the human, and only falls back to an executive (excluding recipient AND asker) when the recipient has no reports_to.

## review-wraith verdict: SHIP
- read-only lookups in the sweep (GetAgent per hop, bounded 8 hops, cycle break); no new writes, no lock change, no schema change
- ACK ladder (resolveAckRung2) untouched; existing answer escalation tests green
- tool schema unchanged

RED_EVIDENCE:
cmd: go test -tags fts5 ./internal/relay -run TestAnswerEscalation
test_sha: 3c54d0d
output:
--- FAIL: TestAnswerEscalationSkipsSender (0.09s)
        obligations_answer_chain_test.go:70: escalated to niwa2 (answer.role), want user (answer.human)
        obligations_answer_chain_test.go:81: role rung on lead (answer.role), want boss (answer.role): the asker is skipped, next up
--- FAIL: TestAnswerEscalationInactiveReportOfSender (0.05s)
    obligations_answer_chain_test.go:98: escalated to niwa2 (answer.role), want user (answer.human)
FAIL
FAIL	agent-relay/internal/relay	0.625s
FAIL

## 3. Files changed

```
internal/relay/cleanup.go                       |  49 +++++++++++-
 internal/relay/obligations_answer_chain_test.go | 102 ++++++++++++++++++++++++
 2 files changed, 150 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `b62b3966-7d67-40fa-9ad1-9375ad7ac74c`._
