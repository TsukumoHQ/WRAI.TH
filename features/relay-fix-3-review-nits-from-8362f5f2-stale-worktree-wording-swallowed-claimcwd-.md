# [relay] fix 3 review nits from 8362f5f2 (stale worktree wording, swallowed ClaimCwd error)

## Team : wraith-engine-2 (agent-relay)
## Branch : wraith-engine-2/cwd-nits (from main)
## Relay task : 4bd9cb95-c995-4bf4-98a1-dfd85f99e284
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. register_agent cwd param description no longer says one-agent-per-worktree
- [ ] 2. onboarding prompt no longer says one agent = one worktree
- [ ] 3. ClaimCwd error is logged, not discarded
- [ ] 4. go build/vet/test -tags fts5 all green

## 2. Root cause & decisions

ROOT_CAUSE: review-8362f5f2 (da6b094, N-agents-per-cwd cohabitation fix) flagged 3 non-blocking findings that shipped as-is because they were wording/logging nits, not correctness bugs: (1) register_agent's cwd param description in tools.go still said "Must be unique per agent (one agent per worktree)", now false; (2) the onboarding prompt in handlers.go still asserted "One agent = one worktree", same staleness; (3) ClaimCwd's error was discarded (`_`) in the register path, so a failed cwd bind silently leaves the agent a cwd-less ghost with zero operator-visible signal.

DECISION: Follow-up-only change, no design decision — reword both stale descriptions to state the new N-agents-per-cwd semantics, and log (non-fatal, registration already succeeded by that point) ClaimCwd's error instead of swallowing it, matching the existing log.Printf pattern already used for the RebindSession ambiguous-cwd case (handlers_ingest.go:131).

REJECTED ALTERNATIVES: none — these are the literal findings from the prior review, applied directly.

[LEGACY_OPPORTUNITY]: none.

ROUND 2 (reviewer-caught miss, triaged by wraith-backend-2): internal/cli/skill.go:80 — the generated onboarding SKILL doc (agent-relay init) — carried the same stale "one agent = one worktree" sentence, missed in round 1. Fixed to match handlers.go:583's wording.

## review-wraith verdict: SHIP
Scope: internal/relay/tools.go (cwd param description), internal/relay/handlers.go (onboarding prompt), internal/relay/handlers_agents.go (log ClaimCwd error), internal/cli/skill.go (generated onboarding doc, round 2)
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt OK / test -tags fts5 OK (567 passed, 12 packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
features/DEBT.md                                   |  1 +
 ...2-stale-worktree-wording-swallowed-claimcwd-.md | 61 ++++++++++++++++++++++
 internal/cli/skill.go                              |  3 +-
 internal/relay/handlers.go                         |  2 +-
 internal/relay/handlers_agents.go                  | 10 +++-
 internal/relay/tools.go                            |  2 +-
 6 files changed, 75 insertions(+), 4 deletions(-)
```

## 4. QA Log

### Round 1 — ❌ REJECTED by review-4bd9cb95-c995-4bf4-98a1-dfd85f99e284
go build/vet -tags fts5 OK, go test 567 pass in 12 pkgs, ClaimCwd/Rebind tests pass; tools.go + handlers.go wording fixed and ClaimCwd error now logged (slog/stderr-safe), BUT internal/cli/skill.go:80 still ships the exact sentence 'one agent = one worktree' in the generated onboarding SKILL doc — same stale claim this task purges, one surface missed; fix that line, resubmit
- 🟢 AC1: stale 'one agent per worktree' gone, replacement is factually correct — evidence: internal/relay/tools.go:38 diff — cwd description now says N agents can share one cwd; claim verified accurate against RebindSession (internal/db/agents.go:384-409: name-given rebind works with shared cwd, cwd-only rebind refuses ambiguous)
- 🔴 AC2: [partial] the named prompt is fixed but the identical stale sentence survives on another onboarding surface shipped to every new machine — purge incomplete — evidence: internal/relay/handlers.go:583 fixed, but grep sweep found internal/cli/skill.go:80 still says 'one agent = one worktree' in the generated onboarding SKILL.md
- 🟢 AC3: error logged not discarded, log path safe for stdio transport — evidence: internal/relay/handlers_agents.go:139-146 — claimErr captured and log.Printf'd, non-fatal; internal/logging/logging.go:41-42 bridges log to slog on stderr so stdio MCP stdout stays clean; TestClaimCwd* pass
- 🟢 AC4: all green as required — evidence: executed: go build -tags fts5 ./... OK, go vet -tags fts5 ./... no issues, go test -tags fts5 ./... 567 passed in 12 packages

### Round 2 — ❌ REJECTED by review-4bd9cb95-c995-4bf4-98a1-dfd85f99e284
go build/vet/test -tags fts5 all green (567 pass, 12 pkgs); AC1/AC3 verified in code, BUT recidivism: internal/cli/skill.go:80 still ships 'one agent = one worktree' — the exact line round 1 rejected on, branch tip unchanged (a2c0777), no commit touches skill.go; fix that one line, resubmit
- 🟢 AC1: cwd param description reworded, matches actual behavior — evidence: internal/relay/tools.go:38 diff; RebindSession internal/db/agents.go:399-409 confirms described ambiguity refusal
- 🔴 AC2: [partial] handlers onboarding prompt fixed but generated onboarding skill doc still ships the stale sentence r1 gated on — evidence: handlers.go:580 fixed; grep found internal/cli/skill.go:80 still 'one agent = one worktree'
- 🟢 AC3: error logged non-fatally, matches package log pattern — evidence: internal/relay/handlers_agents.go:141-146 log.Printf on claimErr, stderr-safe
- 🟢 AC4: executed locally in worktree — evidence: ran go build/vet/test -tags fts5: build OK, vet clean, 567 tests pass in 12 packages

## 5. Timeline

- round 1 → **reject** (review-4bd9cb95-c995-4bf4-98a1-dfd85f99e284)
- round 2 → **reject** (review-4bd9cb95-c995-4bf4-98a1-dfd85f99e284)

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `4bd9cb95-c995-4bf4-98a1-dfd85f99e284`._
