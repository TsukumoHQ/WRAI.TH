# [wraith/relay][split] R1 session_context budget: constraints under the memory budget (cap 8), previews 400->200, decisions 5 @300, register_agent(session_context='minimal') — 20 KB -> <5 KB per register

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/session-ctx-budget (from main)
## Relay task : 38c10034-9a17-481a-94a7-55c7b76f11ac
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 constraints no longer bypass the budget: with 20 constraints × 400 chars and sessionMemoryBudget=6000, relevant_memories carries exactly the 8 most recent constraints plus whatever fits, memories_omitted = the exact remainder (test)
- [ ] 2. AC2 previews: memory value_preview ≤200 chars, decisions ≤5 entries with decision/rationale ≤300 chars, decisions_omitted exact (test)
- [ ] 3. AC3 register_agent(session_context='minimal') and get_session_context(session_context='minimal') return only agent + pending_tasks + unread_messages + cross_project_unread; default = 'full' (test asserts key sets)
- [ ] 4. AC4 byte targets on the fixture (20 constraints, 60 decisions, 10 unread, 5 tasks): full ≤7680 B, minimal ≤3584 B (test asserts len(json)) — amended from 5120/1536 (dispatcher ruling 21:33Z: floor-8 memories = 3145 B and 10 unread summaries = 2091 B are irreducible without a field-set trim, which is a follow-up ticket)
- [ ] 5. AC5 TestToolSchemaBudget green: register_agent ≤2300 B, total ≤57344 B (numbers cited); go test -tags fts5 ./... green (count cited); zero schema change; ≤5 NON-TEST source files (tests + scribe md excluded from the count — dispatcher ruling 21:21Z, handlers split across handlers_agents.go/handlers_context.go)

## 2. Root cause & decisions

# R1 session_context budget cut

ROOT_CAUSE: `register_agent` / `get_session_context` replies were ~16–20 KB because
`internal/relay/project.go` let `layer='constraints'` memories BYPASS the soft
memory budget wholesale — the fleet writes nearly everything as constraints, so
`relevant_memories` returned 14–20 fat entries (~5.5 KB, budget dead). Compounded
by `sessionDecisionMax=40` @ `decisionPreview=400` (~7 fat multi-line decisions)
and no lean boot option.

## Decision

- Guaranteed floor instead of blanket bypass: `sessionConstraintFloor=8` — the
  first 8 constraints (ListBootMemories orders constraints first, updated_at DESC)
  always surface; beyond the floor constraints obey `sessionMemoryBudget` like any
  other memory.
- Previews/caps: `memValuePreview 400→200`, `decisionPreview 400→300`,
  `sessionDecisionMax 40→5`.
- Section budgets lowered so the caps actually bind on a constraint-heavy fleet:
  `sessionMemoryBudget 6000→2600` (at 6000 ~14 fat constraints still surfaced —
  the leak persisting), `sessionDecisionBudget 6000→2000`. Overflow reachable via
  `get_memory` / `recall_decisions`. `*_omitted` counters stay exact.
- Opt-in lean shape: `buildSessionContext(..., minimal ...bool)` +
  `session_context` param (full|minimal) on `register_agent` and
  `get_session_context`. Minimal = agent + pending_tasks + unread_messages +
  cross_project_unread only. Variadic so existing three-arg callers keep compiling.
- Zero schema change.

## Measured (fixture: 20 constraints ×400 char, 60 decisions, 10 unread, 5 tasks)

- full boot payload: 7477 B (target ≤7680)
- minimal boot payload: 3365 B (target ≤3584)
- tool schemas total: 57219 B (≤57344); register_agent 2081 B (≤2300)

## Amendments (wraith-cto, audited)

- AC5: "≤5 files" = ≤5 NON-TEST source files (tests + scribe md excluded). Register
  and get_session_context live in handlers_agents.go / handlers_context.go, not
  handlers.go as the ticket assumed — minimal source touch is exactly 5.
- AC4 (option A): original ≤5120 B (full) / ≤1536 B (minimal) were unreachable —
  floor-8 fat constraints (~3.1 KB) + 10 unread summaries (~2.1 KB, metadata-heavy:
  id/from/to/type/priority/thread/created_at/subject) + 5 tasks (~1.2 KB) are the
  irreducible floor for this scope. Amended to full ≤7680 B / minimal ≤3584 B.

## Rejected alternatives

- Keep `sessionMemoryBudget=6000` (AC1's stated value): leaves relevant_memories at
  ~5.5 KB — the leak persists. Rejected; 2600 is the actual fix.
- Trim per-item message/task summary field sets to hit the original ≤5120/≤1536:
  real savings but new surface — split to a separate follow-up ticket by wraith-cto
  (do NOT fold in).
- Shrink the AC4 fixture counts to match the old targets: games the assertion.
  Rejected.

## review-wraith verdict: SHIP
Scope: internal/relay/{project.go,handlers.go,tools.go,handlers_agents.go,handlers_context.go} + project_test.go + handlers_test.go. Pure boot-payload projection + a session_context param; no DB write, no schema/migration, no new route, no delivery/inbox/updater change.
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 ./... OK (all packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- tools.go: session_context advertises enum full|minimal but the handlers treat any value != "minimal" as full (fail-open to the safe default). Intentional; strict enum rejection not worth the surface.
- Behavior change: full-mode boots now surface ~8 constraints + a small budgeted remainder instead of ~14 (sessionMemoryBudget 6000→2600); overflow reachable via get_memory. Intended leak fix, approved by wraith-cto.

## 3. Files changed

```
internal/relay/handlers.go         |  20 +++++-
 internal/relay/handlers_agents.go  |   7 +-
 internal/relay/handlers_context.go |   5 +-
 internal/relay/handlers_test.go    | 143 +++++++++++++++++++++++++++++++++++++
 internal/relay/project.go          |  52 ++++++++++----
 internal/relay/project_test.go     |  91 ++++++++++++++++++++++-
 internal/relay/tools.go            |   7 ++
 7 files changed, 304 insertions(+), 21 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `38c10034-9a17-481a-94a7-55c7b76f11ac`._
