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

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/relay/toolset.go (discover_tools terse), handlers_messaging.go + budget.go (get_inbox budget default, held_back), tools.go/handlers.go/handlers_ingest.go (descriptions/hints), tests (inbox_budget_default_test, compact_echo_test, toolset_test), CHANGELOG.md, skill/relay.md
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (8 packages ok, 82ad3e8)

BLOCKERS (must fix before merge):
- none. No schema/migration, no agentColumns/scanAgent touch. No new write on the poll path: budget path = FetchInboxDeliveries + MarkDeliveriesSurfaced(survivors), the same single UPDATE GetInboxViaDeliveries already does (deliveries.go:80), empty list = no-op. Inbox stays non-destructive (pruned deliveries stay queued; TestInboxBudget_PrunedStayUnread drains all 6 by mark_read). P0 never pruned, first-surfacing full-content rule untouched. Tool registry single source; stdio/HTTP parity unchanged.

NITS (non-blocking):
- Behavior change fleet-wide: every agent (column default 16384) now gets a pruned inbox when unread > 16 KB. held_back makes it visible; agents that never mark_read see the same top-scored subset until they do.
- discover_tools default no longer lists optional params or types/enums; a client that built calls from the old compact form must pass verbose=true.
- CHANGELOG [Unreleased] section added; cto folds it at release.

## 3. Files changed

```
CHANGELOG.md                                |   7 ++
 internal/relay/budget.go                    |  10 ++
 internal/relay/compact_echo_test.go         | 105 +++++++++---------
 internal/relay/handlers.go                  |   2 +-
 internal/relay/handlers_ingest.go           |   2 +-
 internal/relay/handlers_messaging.go        |  40 +++++--
 internal/relay/inbox_budget_default_test.go | 162 ++++++++++++++++++++++++++++
 internal/relay/tools.go                     |   2 +-
 internal/relay/toolset.go                   | 144 +++++++++----------------
 internal/relay/toolset_test.go              |  15 +--
 skill/relay.md                              |   6 +-
 11 files changed, 330 insertions(+), 165 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `6ead6bed-c274-46f1-8bad-f233fc8bec46`._
