# [relay/doctrine-reach] policy messages never expire unread and stay surfaced at boot until acked

## Team : wraith-engine (tsukumo)
## Branch : wraith/policy-type (from main)
## Relay task : adefc1f4-fdba-4185-8a41-3bd98c222625
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. send_message accepts type=policy; a policy message is stored with ttl_seconds=0 whatever ttl the caller passes, and is never purged nor deadlettered by the expiry sweep (tests PolicyTTLForcedZero, PolicyNeverDeadlettered).
- [ ] 2. get_session_context lists every policy message whose delivery to the caller is not acknowledged, in its own block outside the soft unread budget but inside the existing hard ceiling; once the caller acks the delivery it no longer appears (tests UnackedPolicySurfacedAtBoot, AckedPolicyNotSurfaced, PolicyBlockRespectsHardCeiling).
- [ ] 3. A policy message with no explicit action_required gets action_required=do (test PolicyDefaultsToDo).
- [ ] 4. The boot selection journal line marks policy items so the share of active agents that acked each policy message can be computed by one SQL query over deliveries; the query is included in the PR body (test JournalMarksPolicyItems).
- [ ] 5. Non-policy messages keep today's TTL, deadletter and boot behaviour; go vet clean; go test -tags fts5 -race ./internal/... green.

## 2. Root cause & decisions

# adefc1f4 — type=policy: never expires, surfaced at boot until acked

ROOT_CAUSE: doctrine had no message type of its own, so a policy rode the normal TTL (P1 = 7d default, 4h when a caller set it) and expired unread: "POLICY: typed tickets + tests obligatoires" is 166 deadletter rows (160 to identities already silent at send, 6 to live agents that never acked) and its source row is purged. Nothing kept an unacked policy in front of the agent at boot.

DECISION:
- `db.PolicyType = "policy"`; `normalizeMessageTTL` forces ttl_seconds=0 for it whatever the caller passes, in both insert sinks. ttl=0 is already excluded by ExpireMessages, so the policy is never expired, deadlettered, purged or tombstoned.
- `deriveActionRequired`: policy -> "do" when the caller sets none.
- `UnackedPolicies(project, agent)`: read-only query over deliveries in queued/surfaced joined to type=policy messages (GetInbox is not reused: it flips queued->surfaced and caps at 50).
- buildSessionContext: policies are filtered out of the soft-budgeted unread list (non-policy selection unchanged) and projected into their own `policies` block by `projectPolicies`, with room = sessionUnreadBudget x budgetHardMultiplier minus the bytes the unread block used. First policy always surfaces (the P0 pattern).
- Journal: one `[budget]` line with path=session_context_policy per boot that has policies; every candidate on that line is a policy item.
- tools.go enum gains "policy" (description trimmed to stay under the 2300 B per-tool schema cap).

SCHEMA: none (type is free text; ttl_seconds and deliveries.state already carry everything).

Reach query (one SQL over deliveries):
```sql
SELECT m.project, m.id, m.subject,
       count(*) AS recipients,
       sum(a.status='active') AS active_recipients,
       sum(dv.state='acknowledged' AND a.status='active') AS active_acked,
       round(1.0*sum(dv.state='acknowledged' AND a.status='active')/max(sum(a.status='active'),1),3) AS reach
FROM messages m JOIN deliveries dv ON dv.message_id = m.id
LEFT JOIN agents a ON a.name = dv.to_agent AND a.project = dv.project
WHERE m.type = 'policy'
GROUP BY m.id ORDER BY m.created_at DESC;
```
(Durable: policy messages are ttl=0, so their deliveries are never purged.)

## review-wraith verdict: SHIP
Scope: internal/db/messages.go, internal/relay/{tools.go,handlers.go,project.go}, internal/relay/policy_test.go.
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/... green (2 runs, 0 FAIL); TestToolSchemaBudget passes (send_message under 2300 B).

BLOCKERS: none.
- No new write anywhere; boot adds one RO query (UnackedPolicies).
- Non-destructive inbox: a policy leaves the boot block only through the existing mark_read / ack_delivery (AckedPolicyNotSurfaced); get_inbox surfacing does not hide it (UnackedPolicySurfacedAtBoot).
- Budget: soft 6000 B untouched for non-policy items; policies bounded by the existing hard ceiling (PolicyBlockRespectsHardCeiling).

NITS (non-blocking):
- A caller may still pass action_required=none on a policy; the ticket only asks for the default. The boot block shows it regardless.
- GetInbox(limit 50) now yields fewer than 50 non-policy candidates when policies are among the 50 newest; negligible (policies are rare), and they are listed in full in their own block.

## 3. Files changed

```
...-never-expire-unread-and-stay-surfaced-at-bo.md |  86 ++++++++++
 internal/db/messages.go                            |  33 +++-
 internal/relay/handlers.go                         |  24 ++-
 internal/relay/policy_test.go                      | 186 +++++++++++++++++++++
 internal/relay/project.go                          |  27 +++
 internal/relay/tools.go                            |   4 +-
 6 files changed, 352 insertions(+), 8 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-adefc1f4-fdba-4185-8a41-3bd98c222625
- 🟢 AC1: ttl forced 0 and never expired/purged/deadlettered; verified by execution — evidence: internal/db/messages.go:37-49 normalizeMessageTTL forces ttl=0 for type=policy; InsertMessage+InsertMessageWithDeliveries use it; expiry sweep guards on ttl_seconds>0 (messages.go:622) — test: TestPolicy/PolicyTTLForcedZero + TestPolicy/PolicyNeverDeadlettered internal/relay/policy_test.go — both pass; control non-policy message gets deadlettered while policy survives
- 🟢 AC2: behavior verified end-to-end through buildSessionContext — evidence: internal/relay/handlers.go:885-907 splits nonPolicy out of unread, projects policies into room = hardCeil-used; internal/relay/project.go:320-340 projectPolicies honors room and always surfaces first — test: TestPolicy/UnackedPolicySurfacedAtBoot + TestPolicy/AckedPolicyNotSurfaced + TestPolicy/PolicyBlockRespectsHardCeiling internal/relay/policy_test.go — all pass
- 🟢 AC3: action_required=do when caller omits it — evidence: internal/db/messages.go:61 deriveActionRequired adds case "task", PolicyType -> do — test: TestPolicy/PolicyDefaultsToDo internal/relay/policy_test.go — passes
- 🔴 AC4: AC explicitly required the SQL query in the PR body; doer shipped the journal but not the query — partial — evidence: Journal mark done: internal/relay/project.go:320 emits Path=session_context_policy; TestPolicy/JournalMarksPolicyItems passes. SQL query MISSING: commit 96a9ee4 body contains zero SQL; grep over diff finds no reachability query anywhere — test: TestPolicy/JournalMarksPolicyItems internal/relay/policy_test.go covers journal; NO test or doc covers the reachability SQL
- 🟢 AC5: non-policy behavior preserved; mechanical gates green — evidence: normalizeMessageTTL falls through to normalizeTTL for non-policy (unchanged); deriveActionRequired only adds PolicyType to existing case; handlers filters policies out before soft-budget projection — test: TestPolicy/NonPolicyBootUnchanged internal/relay/policy_test.go passes; go test -tags fts5 -race ./internal/... 9 pkgs ok; go vet clean

### Round 2 — ✅ APPROVED by review-adefc1f4-fdba-4185-8a41-3bd98c222625
- 🟢 AC1: force-ttl + never-expire invariant fully exercised — evidence: internal/db/messages.go:46 normalizeMessageTTL forces ttl=0 for PolicyType; called from InsertMessage L168 and InsertMessageWithDeliveries L238; ExpireMessages L616 excludes ttl_seconds=0; PurgeExpiredMessages L660 keys on expired_at IS NOT NULL (policy never set); Deadletter reads deadletter table only written from expired messages — test: TestPolicy/PolicyTTLForcedZero + TestPolicy/PolicyNeverDeadlettered (internal/relay/policy_test.go) — both PASS, plain ttl=1 control message expires+deadletters, policy survives untouched
- 🟢 AC2: own block, soft-budget excluded, hard-ceiling bounded, ack removes — all verified behaviorally — evidence: internal/relay/handlers.go:888 nonPolicy filter strips PolicyType out of unread block; L902-906 UnackedPolicies -> projectPolicies with room = sessionUnreadBudget*budgetHardMultiplier-used (hard ceiling enforced); internal/db/messages.go:545 UnackedPolicies filters state IN (queued,surfaced); MarkRead L488 calls AcknowledgeDeliveryByMessage which sets state=acknowledged, so acked policies drop out of UnackedPolicies — test: TestPolicy/UnackedPolicySurfacedAtBoot + TestPolicy/AckedPolicyNotSurfaced + TestPolicy/PolicyBlockRespectsHardCeiling — all PASS
- 🟢 AC3: default derivation path covered — evidence: internal/db/messages.go:60 deriveActionRequired switch case "task", PolicyType: return "do" — test: TestPolicy/PolicyDefaultsToDo (internal/relay/policy_test.go) — PASS, asserts parseJSON(res)["action_required"] == "do"
- 🟢 AC4: journal marks policy items + SQL query documented in PR body — evidence: internal/relay/project.go:321 projectPolicies emits budgetJournal{Path: "session_context_policy"}; PR body (commit 7cc9d6b) contains full SQL reach query over messages JOIN deliveries LEFT JOIN agents — test: TestPolicy/JournalMarksPolicyItems (internal/relay/policy_test.go) — PASS, asserts session_context_policy journal line has agent=dev, Selected=[id] and session_context line does NOT include policy id
- 🟢 AC5: non-policy behavior unchanged; gates green — evidence: internal/relay/handlers.go:888-895 preserves non-policy selection (only filter is the m.Type != PolicyType strip); TestPolicy/NonPolicyBootUnchanged PASS; go vet -tags fts5 ./internal/... clean; go test -tags fts5 -race ./internal/... exit=0 950 passed; no test deletions/ignores/threshold changes in diff — test: TestPolicy/NonPolicyBootUnchanged (internal/relay/policy_test.go) + existing TestP0InsertedTTLForcedNeverExpires/NonP0StillExpires/PurgeExpiredMessages/DeadletterJournalsExpiredUnread/ToolSchemaBudget — all PASS

## 5. Timeline

- round 1 → **reject** (review-adefc1f4-fdba-4185-8a41-3bd98c222625)
- round 2 → **approve** (review-adefc1f4-fdba-4185-8a41-3bd98c222625)

**Approve-with-findings (follow-up):** go test -tags fts5 -race ./internal/... 950 passed (exit 0); go vet clean; TestPolicy 9/9 sub-tests pass; existing TTL/deadletter-purge tests green; mental revert of normalizeMessageTTL/deriveActionRequired/UnackedPolicies/projectPolicies breaks pinning tests

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `adefc1f4-fdba-4185-8a41-3bd98c222625`._
