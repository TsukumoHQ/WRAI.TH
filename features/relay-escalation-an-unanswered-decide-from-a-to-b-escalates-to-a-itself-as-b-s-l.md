# [relay/escalation] an unanswered 'decide' from A to B escalates to A itself as 'B's lead' (chain walks the wrong way)

## Team : wraith-backend (tsukumo)
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
test_sha: 06f0c8c
output:
--- FAIL: TestAnswerEscalationSkipsSender (0.27s)
        obligations_answer_chain_test.go:70: escalated to niwa2 (answer.role), want user (answer.human)
        obligations_answer_chain_test.go:81: role rung on lead (answer.role), want boss (answer.role): the asker is skipped, next up
--- FAIL: TestAnswerEscalationInactiveReportOfSender (0.12s)
    obligations_answer_chain_test.go:98: escalated to niwa2 (answer.role), want user (answer.human)
FAIL
FAIL	agent-relay/internal/relay	0.940s
FAIL

## 3. Files changed

```
...e-from-a-to-b-escalates-to-a-itself-as-b-s-l.md |  68 ++++++++++++++
 internal/relay/cleanup.go                          |  49 +++++++++-
 internal/relay/obligations_answer_chain_test.go    | 102 +++++++++++++++++++++
 3 files changed, 218 insertions(+), 1 deletion(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-b62b3966-7d67-40fa-9ad1-9375ad7ac74c
- 🟢 AC1: AC1 behavior (escalation lands on F when chain is A->B->F) is verified end-to-end: the obligation breaches and opens answer.role on boss. The test is a regression guard; it does not pin the original bug but it does assert AC1's spec. — evidence: internal/relay/obligations_answer_chain_test.go:46-56: dev reports_to mid, mid reports_to boss; after 61min sweep, roleBearer returns bearer='boss', norm='answer.role'. Test PASS with fix; also PASSES with old resolveAckRung2 (1-hop already returned boss). — test: TestAnswerEscalationClimbsRecipientChain internal/relay/obligations_answer_chain_test.go:46
- 🟢 AC2: Two sub-cases both pin the asker-skipping behavior. Pinning verified by reversion: 2/2 fail under old resolver. assertNoMessageTo also confirms no relay message was sent to the asker. — evidence: internal/relay/obligations_answer_chain_test.go:59-84: subtest 1 sets cto.ReportsTo='founder' and asker=niwa2 (active executive), expects escalation to user/answer.human; subtest 2 sets dev.ReportsTo=lead (asker) and expects escalation to boss, skipping lead. With fix reverted to resolveAckRung2, both subtests FAIL (escalated to niwa2 / lead respectively). — test: TestAnswerEscalationSkipsSender internal/relay/obligations_answer_chain_test.go:59
- 🟢 AC3: Inactive agent whose reports_to points at the asker does not make the asker the recipient's lead. Pinning verified: this test FAILS with the old resolveAckRung2 (returns niwa2), PASSES with the new resolveAnswerRung (returns user). assertNoMessageTo for both niwa1 and niwa2 confirms no spurious relay traffic. — evidence: internal/relay/obligations_answer_chain_test.go:91-101: niwa1 has reports_to=niwa2 (the asker) and status='inactive'. The walk must skip the inactive niwa1 and not bounce escalation back to niwa2. With fix reverted, escalation lands on niwa2 (the asker) via the executive-fallback; with the fix, lands on user/answer.human. — test: TestAnswerEscalationInactiveReportOfSender internal/relay/obligations_answer_chain_test.go:91
- 🔴 AC4: AC4 explicitly requires 'root cause named file:line in the PR body'. The doer's commit body is empty; the code comment identifies the function name but not a file:line. The gate's scribe added the file:line retroactively. The doer did not satisfy this AC. — evidence: git show 78011a0 --format=%B prints only the subject; the commit body is empty. The fix commit's PR body (commit body) does not name a root cause file:line. The code comment in cleanup.go:635-641 names the function resolveAckRung2 and the bug, but no specific file:line. The scribe provenance doc d285e31 (gate-authored, post-submission) does include ROOT_CAUSE: internal/relay/cleanup.go:651 — but the doer did not author that. — test: NONE — AC4 is a docs/PR-body requirement, not behavior
- 🟢 AC5: Machine verify matches AC5; no regression in the surrounding escalation/ack machinery. — evidence: go test -tags fts5 ./internal/... exit 0, all packages OK (cli, config, connector/linear, db, ingest, normalize, relay). Verified directly: full suite green; 5 TestAnswer* pass; prior TestAnswerBreachOpensRoleChildOnce, TestAnswerHumanOnlyAtMaxDepth, TestAnswerRoleChildFulfilledAckUntouched, TestAnswerObligationAskFulfilledByReply, TestAnswerObligationDecideDeclineAndTombstone, TestAnswerObligationNoneForNonAskAndBroadcast still pass — no regression in the ACK ladder or answer obligation engine. — test: go test -tags fts5 ./internal/... (full suite)

### Round 2 — ❌ REJECTED by human:cto-tsukumo

## 5. Timeline

- round 1 → **reject** (review-b62b3966-7d67-40fa-9ad1-9375ad7ac74c)
- round 2 → **reject** (human:cto-tsukumo)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `b62b3966-7d67-40fa-9ad1-9375ad7ac74c`._
