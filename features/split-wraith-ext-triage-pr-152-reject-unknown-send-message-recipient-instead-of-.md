# [split][wraith/ext-triage] PR #152 reject unknown send_message recipient instead of silent-accept — rebase on main, review, gate

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/ext-152 (from main)
## Relay task : 54ea39d8-1bbd-4efe-a363-b9792b5d0d61
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestSendMessageUnknownRecipientRejected: send_message to a name that is neither an agent, a team, '*', 'human'/'user' nor a federation peer in the project returns an error naming the recipient and inserts no message row
- [ ] 2. AC2 TestSendMessageKnownNonAgentRecipientsStillAccepted: '*' broadcast, an existing team name, 'human' and 'user' each still create the message + deliveries exactly as on main d35ab8c
- [ ] 3. AC3 TestExtPR152DiffScope: go test -tags fts5 ./internal/relay/... ./internal/db/... green; diff limited to the PR's files (+ one test file); original author kept as commit author

## 2. Root cause & decisions

# T54ea39d8 — ext-triage PR #152: reject unknown send_message recipient

## Root cause
send_message accepted ANY `to`: a name nobody registered returned a message-id,
zero deliveries were written, nobody was ever alerted — a silent black hole
(the CTO's live incident). External PR #152 (author Jérôme Chincarini) adds an
up-front guard that rejects an unknown recipient with a suggestion.

## Decision
Rebase PR #152 onto main 45090bf, keep the author's commit + authorship, resolve
the one handlers.go conflict, then fix two regressions the guard introduced and
add the missing AC coverage — all in a SEPARATE commit on top of the author's.

Conflict (handlers.go sendCrossProject): main renamed mcp.NewToolResultError →
the canonical `toolResultError` envelope; kept the PR's richer guard
(unknownCrossProjectRecipientError + soft-deleted check) expressed via that helper.

Regressions fixed (my commit 8e4b8b1, on top of the author's 207b9b1):
1. `human`: the console operator inbox, twin of `user` (resolveTargets maps both
   to the same target). The unconditional guard rejected it — now exempted like
   `user`. (Aligns with the incoming operator-identity work, task ea0cc293.)
2. Fleet-expected boot-race: a name dispatched a task (profile_slug=name) before
   it registers must QUEUE, not be rejected (task 0464d6cb) — the guard now
   defers to RecipientIsFleetExpected before rejecting a nil row, mirroring the
   permission block below. This restored the pre-existing
   TestSendMessageQueuesForFleetExpectedRecipient which the blanket reject broke.
3. Envelope consistency: the author's new guard used raw mcp.NewToolResultError,
   bypassing the canonical typed error envelope every other path uses
   (errors.go) — routed through toolResultError.

## Requirement review (task-mandated, messaging surface)
- (i) unknown recipient rejected, error names it + suggestion — YES (guard + suggest.go).
- (ii) non-agent recipients still work: `*`, `team:<slug>`, `user`, `human`,
  fleet-expected boot-race — YES (exemptions + fleet-expected fix), pinned by
  TestSendMessageKnownNonAgentRecipientsStillAccepted.
- (iii) "did you mean" never leaks another project's roster — YES:
  unknownRecipientError uses ListAgents(caller project),
  unknownCrossProjectRecipientError uses ListAgents(target project); pinned by
  TestExtPR152DiffScope.

## Original failing check
The PR's one red check was goimports/gofmt drift on migrate_projects_test.go
under go1.25 (fixed by the author's 2nd commit f48538c). That gofmt change is
already upstream on main, so the rebase DROPPED that commit ("patch contents
already upstream"); migrate_projects_test.go is untouched here.

## ACs → tests
- AC1 unknown rejected, names recipient, no row → TestSendMessageUnknownRecipientRejected (author's)
- AC2 *, team, user, human still accepted → TestSendMessageKnownNonAgentRecipientsStillAccepted
- AC3 build+test green, scope, author kept, no cross-project leak → TestExtPR152DiffScope

## review-wraith verdict: SHIP
Scope: internal/relay/handlers.go, handlers_messaging.go, handlers_test.go, suggest.go, NEW ext_pr152_recipient_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 relay+db OK (819 pass)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- Federation-peer / bare-role (`cto`/`founder`) recipients: not delivered through this
  bare-`to` path in current code, so unaffected by the guard; not separately tested.
  Flagged to cto — if a future path addresses them literally, revisit the exemption set.

## 3. Files changed

```
...t-unknown-send-message-recipient-instead-of-.md |  99 ++++++++++++
 internal/relay/ext_pr152_recipient_test.go         |  81 ++++++++++
 internal/relay/handlers.go                         |  10 +-
 internal/relay/handlers_messaging.go               |  32 ++++
 internal/relay/handlers_test.go                    | 170 +++++++++++++++++++++
 internal/relay/suggest.go                          | 170 +++++++++++++++++++++
 6 files changed, 560 insertions(+), 2 deletions(-)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-54ea39d8-1bbd-4efe-a363-b9792b5d0d61 @ `05d21398f`
- 🟢 AC1: guard fires on GetAgent==nil or status==deleted, message row never inserted because error returns before insert path — evidence: handlers_messaging.go:122-141 early-returns toolResultError(unknownRecipientError(...)) before InsertMessageWithDeliveries; error names the recipient — test: TestSendMessageUnknownRecipientRejected internal/relay/handlers_test.go:404-415
- 🟢 AC2: test asserts no IsError and a persisted message id for all four recipients, plus inbox delivery for broadcast/team — evidence: handlers_messaging.go:122 exempts to in {*, user, human, team:*}; human/user hit ResolveRecipients direct path, team: hits team-fanout branch — same as baseline d35ab8c — test: TestSendMessageKnownNonAgentRecipientsStillAccepted internal/relay/ext_pr152_recipient_test.go:13-58
- 🟢 AC3: behavioral half pins project-scoped ListAgents (no cross-project leak); structural halves (scope + author + green build) verified at the gate — evidence: diff 4 PR source files + 1 added test file matches plan files: scope OK; original author Jérôme Chincarini preserved on 207b9b1; validate command green — test: TestExtPR152DiffScope internal/relay/ext_pr152_recipient_test.go:60-81 + go test ./internal/relay/... ./internal/db/... exit=0

## 5. Timeline

- round 1 → **approve** (review-54ea39d8-1bbd-4efe-a363-b9792b5d0d61)

**Approve-with-findings (follow-up):** go test -tags fts5 ./internal/relay/... ./internal/db/... green (819 passed); AC1 reject guard at handlers_messaging.go:122-141 pins TestSendMessageUnknownRecipientRejected; AC2 'human'+'user' exemption at handlers_messaging.go:122 + RecipientIsFleetExpected deferral at 132-135 pin TestSendMessageKnownNonAgentRecipientsStillAccepted (4 cases: *, team:dev, user, human) + TestSendMessageQueuesForFleetExpectedRecipient; AC3 unknownRecipientError project-scoped ListAgents pins TestExtPR152DiffScope cross-project leak; original author jchincarini preserved on 207b9b1; diff 4 PR source files + 1 added test file matches plan

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `54ea39d8-1bbd-4efe-a363-b9792b5d0d61`._
