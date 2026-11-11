# [audit T5] compact boot payloads: discover_tools terse by default, re-register minimal, inbox budgeted

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/6ead6bed-boot-payloads (from main)
## Relay task : 6ead6bed-c274-46f1-8bad-f233fc8bec46
## Trace : trace=c483f901da7cda7af62d009fcf504dbd
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 discover_tools(category, verbose=false default) returns name + one-line description + required params (<= 1.5k per category); verbose=true returns today's full schema; handler tests
- [ ] 2. AC2 register_agent on an existing agent (re-register) defaults session_context to minimal; first registration unchanged; test
- [ ] 3. AC3 get_inbox apply_budget defaults true for an agent with MaxContextBytes set; non-destructive inbox non-regression test
- [ ] 4. AC4 CHANGELOG + relay skill document verbose and session_context

## 2. Root cause & decisions

# 6ead6bed — [audit T5] compact boot payloads

## Root cause
ROOT_CAUSE: a reboot pays ~32k of relay tool_result because discover_tools returned the full inputSchema of every tool in a category (toolset.go), get_inbox never applied the agent's context budget unless asked (apply_budget default false), and the re-register session_context (already fixed on main by ffd35635) was the third leg.

## AC1 discover_tools terse default (toolset.go HandleDiscoverTools)
- Default payload: {category, params_format, tools{name: "(required params) one-line description"}}; description cut to 60B (discoverToolDescMax), params_format stated once. verbose=true unchanged (full schemas).
- Bytes per category, terse / verbose: tasks 1381 / 13810, messaging 705 / 7662, memory 689 / 7886, all others < 600. All categories terse 6.5k vs previous compact default 24.6k.
- Tests: compact_echo_test.go TestDiscoverTerse_SizeAndRequired (every category <= 1500B, every required param present), TestDiscoverTerse_OneLine; toolset_test.go TestDiscoverTools (terse + verbose halves).

## AC2 re-register minimal — pre-existing on main
- Shipped by ffd35635 (dba9a15): handlers_agents.go:142 wasLive, :200 minimalCtx. Test already on main: compact_echo_test.go TestRegister_ReRegisterOfLiveAgentDefaultsMinimal (first boot full, live re-register minimal, explicit full wins, comeback from inactive full). No code change here.

## AC3 get_inbox budget default on (handlers_messaging.go HandleGetInbox)
- apply_budget omitted => on when agent.MaxContextBytes > 0 (column default 16384, so every agent unless set to 0). Explicit apply_budget wins either way.
- Defaulted mode prunes only when inboxBytes(messages) > budget: an inbox that fits is returned whole, with no [budget] journal line per poll.
- Budget path = FetchInboxDeliveries (no surfacing) + MarkDeliveriesSurfaced(survivors only), the existing 2-step flow: pruned deliveries stay queued => still unread (non-destructive). P0 always kept (applyBudget).
- Pruning is never silent (found in self-review): response carries held_back + held_back_hint (JSON) / header line "(N more held back by the context budget, still unread: ...)" (markdown). Without it, an agent that peeks without mark_read could never learn messages were pruned.
- One GetAgent read per poll moved up front (was inside the budget branch); no new write on the poll path (GetInbox already surfaced on the non-budget path).
- Tests: inbox_budget_default_test.go — DefaultOnPrunesOverBudget, ExplicitFalseWins, PrunedStayUnread (drain by mark_read, all 6 delivered once), FitsReturnsAll, P0NeverPruned, ZeroBudgetDisablesDefault.

## AC4 docs
- CHANGELOG.md: [Unreleased] section (terse discover_tools, inbox budget default, minimal re-register).
- handlers.go / handlers_ingest.go discovery hints: discover_tools lists required params, verbose=true for full schema.
- skill/relay.md: register_agent session_context full/minimal; discover_tools terse + verbose; inbox budget default + apply_budget=false; tools.go apply_budget description updated.

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/ -run TestInboxBudget -count=1   (handlers_messaging.go reverted to origin/main b1d3b6f)
  test_sha: 82ad3e8
  output: |
    --- FAIL: TestInboxBudget_DefaultOnPrunesOverBudget (0.09s)
        inbox_budget_default_test.go:52: default get_inbox should prune to the budget: got 6 of 6
    --- FAIL: TestInboxBudget_PrunedStayUnread (0.08s)
        inbox_budget_default_test.go:74: precondition: default poll should prune, got 6 of 6
    FAIL	agent-relay/internal/relay	1.548s
  AC1 red: on b1d3b6f the default (compact) discover_tools payload for tasks was 7234B > the 1500B bound TestDiscoverTerse_SizeAndRequired asserts.

## r1 findings addressed (eab1068)
- W1 skill/tools-reference.md:11: rewritten to the terse shape ({tools:{name:"(required) desc"}}, e.g. send_message) + get_inbox budget default/held_back line.
- W2 handlers_messaging.go: agent row via h.identityAgent (3s cache + one deadline retry, same read guardIdentity uses) — no uncached reader query per poll on a cache hit. Read error => budget stays on at defaultInboxBudgetBytes (16384, column default) + [budget] log line; not-found (nil,nil) unchanged (no prune). Test TestInboxBudget_FailedAgentReadKeepsDefaultBudget (identityLookup forced to DeadlineExceeded): mutation fallback=0 => FAIL "got 30 of 30"; restored => PASS. Trade-off: a re-register that changes max_context_bytes takes up to identityCacheTTL (3s) to apply.
- N1 CHANGELOG.md: says held-back messages come through only after mark_read of the returned ones; same subset until then.
- N2 README.md:400 + :775: budget on by default, apply_budget:false for the whole inbox, held_back + mark_read behavior.
Gate on eab1068: gofmt OK / vet OK / go test -tags fts5 ./... 8 packages ok.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/relay/toolset.go (discover_tools terse), handlers_messaging.go + budget.go (get_inbox budget default, held_back), tools.go/handlers.go/handlers_ingest.go (descriptions/hints), tests (inbox_budget_default_test, compact_echo_test, toolset_test), CHANGELOG.md, skill/relay.md
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (8 packages ok, eab1068)

BLOCKERS (must fix before merge):
- none. No schema/migration, no agentColumns/scanAgent touch. No new write on the poll path: budget path = FetchInboxDeliveries + MarkDeliveriesSurfaced(survivors), the same single UPDATE GetInboxViaDeliveries already does (deliveries.go:80), empty list = no-op. Inbox stays non-destructive (pruned deliveries stay queued; TestInboxBudget_PrunedStayUnread drains all 6 by mark_read). P0 never pruned, first-surfacing full-content rule untouched. Tool registry single source; stdio/HTTP parity unchanged.

NITS (non-blocking):
- Behavior change fleet-wide: every agent (column default 16384) now gets a pruned inbox when unread > 16 KB. held_back makes it visible; agents that never mark_read see the same top-scored subset until they do.
- discover_tools default no longer lists optional params or types/enums; a client that built calls from the old compact form must pass verbose=true.
- CHANGELOG [Unreleased] section added; cto folds it at release.

## 3. Files changed

```
CHANGELOG.md                                       |   7 +
 README.md                                          |   6 +-
 features/CHANGELOG.md                              |  22 +++
 ...ver-tools-terse-by-default-re-register-minim.md |  93 ++++++++++
 ...-seconds-never-fails-an-authed-call-and-the-.md |  39 +++--
 internal/relay/budget.go                           |  15 ++
 internal/relay/compact_echo_test.go                | 105 ++++++------
 internal/relay/handlers.go                         |   2 +-
 internal/relay/handlers_ingest.go                  |   2 +-
 internal/relay/handlers_messaging.go               |  57 ++++++-
 internal/relay/inbox_budget_default_test.go        | 189 +++++++++++++++++++++
 internal/relay/tools.go                            |   2 +-
 internal/relay/toolset.go                          | 144 ++++++----------
 internal/relay/toolset_test.go                     |  15 +-
 skill/relay.md                                     |   6 +-
 skill/tools-reference.md                           |   3 +-
 16 files changed, 525 insertions(+), 182 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-6ead6bed-c274-46f1-8bad-f233fc8bec46
- 🟢 AC1: Terse-by-default, name + one-line description + required params, <=1.5k per category, verbose=true unchanged, handler tests present and RED without the change. — evidence: internal/relay/toolset.go:571-616 (default branch builds the name->'(required params) description' map; verbose branch unchanged), :641 discoverTerseMaxBytes=1500, :658-688 oneLine/requiredParams/terseTool. Executed: go test -tags fts5 ./internal/relay/ -run 'TestDiscoverTerse|TestDiscoverTools|TestDiscoverPayloadSize|TestDiscoverSchemasValidJSON|TestTokenReductionDiscoveryVsFull' -count=1 => PASS; the test logged terse per category 217B(projects)..1381B(tasks) vs verbose 1244B..13810B, every category <= 1500B. Mutation check in an out-of-tree copy (/tmp/rev6ead/mut_discover, default forced non-terse): TestDiscoverTerse_SizeAndRequired FAILS 'terse payload 3112B > 1500B' and TestDiscoverTools panics on the shape change - the test really pins the default. — test: TestDiscoverTerse_SizeAndRequired internal/relay/compact_echo_test.go:189 (behavioural: calls HandleDiscoverTools per category, measures the payload, and cross-checks each terse line's '(required,params)' prefix against the verbose schema's Required list) + TestDiscoverTools internal/relay/toolset_test.go:14 + TestDiscoverTerse_OneLine internal/relay/compact_echo_test.go:259; verbose=true keeps the full schemas, pinned by the pre-existing TestDiscoverSchemasValidJSON internal/relay/toolset_test.go:162
- 🟢 AC2: Re-register of a live agent already defaults to minimal and the first registration is unchanged; verified by a pre-existing passing test, mutation-proven to be pinned. — evidence: No runtime change in this diff for register_agent - the behaviour pre-exists on origin/main (git show origin/main:internal/relay/handlers_agents.go -> wasLive at :142, minimalCtx at :200, identical to the tip). The diff documents it (skill/relay.md:44, CHANGELOG.md:14). Exception claimed: the criterion is covered by a PRE-EXISTING test the diff leaves passing. Executed: go test -tags fts5 ./internal/relay/ -run TestRegister_ReRegisterOfLiveAgentDefaultsMinimal -count=1 => PASS on the tip; mutation in /tmp/rev6ead/mut_register (wasLive forced false) => FAILS 're-register of a live agent must default to minimal', so the pre-existing test really pins it. — test: TestRegister_ReRegisterOfLiveAgentDefaultsMinimal internal/relay/compact_echo_test.go:137 - pre-existing (present on origin/main at compact_echo_test.go:136), unchanged by this diff; it pins first boot = full, re-register of a live agent = minimal, explicit session_context=full = full, comeback-after-deactivate = full
- 🟢 AC3: apply_budget defaults on for an agent with a budget, an explicit value wins, pruned messages stay unread, and the non-destructive inbox is pinned by a behavioural test. — evidence: internal/relay/handlers_messaging.go:443-452 (budgetExplicit detection, default-on when agentObj.MaxContextBytes>0, explicit value always wins), :489-497 (applyBudget + heldBack), :585-599 (held_back / held_back_hint / markdown header), internal/relay/budget.go:18-24 (inboxBytes, the same per-message measure applyBudget uses). Executed: go test -tags fts5 ./internal/relay/ -run TestInboxBudget -count=1 => PASS (5 tests); mutation in /tmp/rev6ead/mut_budget (default-on block removed) => TestInboxBudget_DefaultOnPrunesOverBudget FAILS 'got 6 of 6' and TestInboxBudget_PrunedStayUnread FAILS its precondition. The pre-existing non-destructive test TestInboxNonDestructivePeek internal/relay/handlers_test.go:702 still passes. — test: TestInboxBudget_DefaultOnPrunesOverBudget internal/relay/inbox_budget_default_test.go:48 + TestInboxBudget_PrunedStayUnread internal/relay/inbox_budget_default_test.go:87 (drains poll by poll with mark_read and asserts all 6 messages eventually surface - the non-destructive guarantee) + TestInboxBudget_ExplicitFalseWins :77 + TestInboxBudget_FitsReturnsAll :119 + TestInboxBudget_P0NeverPruned :136 + TestInboxBudget_ZeroBudgetDisablesDefault :156
- 🔴 AC4: [partial] The two named artifacts document verbose and session_context, but the skill package's tools-reference.md:11 was left describing the payload format this diff replaced, so the skill as installed misdescribes the contract it documents; the CHANGELOG's held-back sentence is also contradicted by execution. — evidence: CHANGELOG.md:9-14 does document verbose and session_context, and skill/relay.md:44 (session_context full/minimal), :102 (inbox budget + apply_budget=false) and :146 (terse discover_tools + verbose=true) do too. But the same shipped skill package still asserts the superseded discover_tools contract at skill/tools-reference.md:11 ('returns compact signatures by default - params: {name: ...}'), verified by executing both payloads (origin/main default = array of {name,description,params}; tip default = {"tools":{"send_message":"(content,subject,to) Send a message"}}), and install.sh:721 installs that file next to relay.md in every agent's skill dir. CHANGELOG.md:10 also overstates the pruned-message behaviour ('come back on a later poll'; by execution the same 3 ids come back on poll1/2/3 and the held-back 3 only surface after mark_read, as skill/relay.md:102 correctly says). — test: NONE - documentation criterion (inherently untestable: prose). The prose was checked by reading it against the executed payload, not by a test.

## 5. Timeline

- round 1 → **reject** (review-6ead6bed-c274-46f1-8bad-f233fc8bec46)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `6ead6bed-c274-46f1-8bad-f233fc8bec46`._
