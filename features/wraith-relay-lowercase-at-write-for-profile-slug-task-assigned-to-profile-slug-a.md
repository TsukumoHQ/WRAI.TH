# [wraith/relay] lowercase-at-write for profile slug, task assigned_to/profile_slug, agent profile_slug — closes the 4 write-path gaps behind the referential scan's LOWER()

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/lowercase-at-write (from main)
## Relay task : df850e85-7d64-4272-95bb-38b5e7121c86
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: register_profile with slug 'Wraith-Backend' stores 'wraith-backend' and get_profile('Wraith-Backend') resolves it
- [ ] 2. AC2 named test: dispatch_task with assigned_to 'Wraith-Backend-2' and profile_slug 'Wraith-Backend' stores both lowercase; update_task assigned_to 'Wraith-Backend-2' reassign stores lowercase
- [ ] 3. AC3 named test: register_agent with profile_slug 'Wraith-Backend' stores 'wraith-backend'
- [ ] 4. AC4 regression: existing handlers tests for profiles/tasks/agents green and a lowercase input at each of the 4 sites produces byte-identical stored rows to today's
- [ ] 5. AC5 scope: diff touches at most handlers_profiles.go, handlers_tasks.go, handlers_agents.go and one test file; no db/ package change, no migration

## 2. Root cause & decisions

ROOT_CAUSE: The referential scan is dropping its LOWER() (ticket B2) to rely on plain equality, but four write paths still stored agent/profile identifiers verbatim — so a future mixed-case register would silently break lookups and the scan. Fix: normalize at write time (lowercase + trim) at all four gap sites, and lowercase the matching lookup inputs so a mixed-case caller still resolves. Handler contract unchanged (same params/responses), no DB or migration change.

## review-agent-runtime verdict: SHIP

```
review-agent-runtime: PASS
Scope: internal/relay/handlers_profiles.go (+5/-2), handlers_tasks.go (+6/-... 2 sites), handlers_agents.go (+2/-1), handlers_lowercase_write_test.go (+129)
BLOCKERS: none
  §1 schema/scan: untouched — no agentColumns/scanAgent edit, no migration, no new column.
  §2 concurrency: no state-transition change; edits only normalize an input string before the existing DB call. No new shared state, no new locks.
  §3 single-writer/availability: observability-neutral — no new writer handle, no DSN change, no new per-request write; get_profile still guards nil profile. get_profile/dispatch/reassign lookup inputs are lowercased so they match the now-lowercased stored rows — the ticket confirms live DB has 0 rows differing from LOWER() on agents.name/tasks.assigned_to/profiles.slug, so no existing mixed-case row is stranded; this normalization IS the enforcement B2 depends on.
  §4 boundary / §6 lifecycle: N/A — no bind/auth/route/updater/shutdown change.
  §5 MCP surface: no new tool (registry untouched), identity resolution (resolveProject/resolveAgent) unchanged; contract additive-safe — only the stored value is normalized, omitted fields still behave as today (optionalStringLower returns nil on "", same as optionalString).
  AC5 scope: diff = exactly the 3 named handlers + 1 test file, no db/ package change, no migration.
NITS: none.
```

Tests (internal/relay): 5 AC tests in handlers_lowercase_write_test.go — TestRegisterProfileLowercasesSlug (AC1: stores lowercase + mixed-case get_profile resolves), TestDispatchTaskLowercasesProfileSlug (AC2 dispatch), TestUpdateTaskReassignLowercasesAssignedTo (AC2 reassign, assigned_to + profile_slug), TestRegisterAgentLowercasesProfileSlug (AC3), TestLowercaseInputByteIdenticalStored (AC4: lowercase input is a no-op across all 4 sites). go test -tags fts5 ./internal/relay/... = 466 pass; -race -count=3 on the 5 new = 15 pass, no data race; go vet + gofmt clean; go build -tags fts5 ./... green.

## 3. Files changed

```
internal/relay/handlers_agents.go               |   2 +-
 internal/relay/handlers_lowercase_write_test.go | 129 ++++++++++++++++++++++++
 internal/relay/handlers_profiles.go             |   5 +-
 internal/relay/handlers_tasks.go                |   6 +-
 4 files changed, 136 insertions(+), 6 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `df850e85-7d64-4272-95bb-38b5e7121c86`._
