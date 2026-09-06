# [relay] registry listings exclude status='deleted' rows (registry-tri #2)

## Team : wraith-backend (niwa)
## Branch : wraith-backend/registry-list-exclude-deleted (from main)
## Relay task : c877a9db-ffb5-4673-8ca3-dfc9504f379d
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 list_agents excludes status='deleted' rows — named test with a mixed live/deleted fixture asserts only live rows returned
- [ ] 2. AC2 every other registry listing surface excludes deleted rows — table-driven named test covering the enumerated surfaces; the enumeration is stated in the report
- [ ] 3. AC3 deleted rows remain in the table untouched (no hard delete, audit trail intact) — named test asserts row still readable directly after being filtered from listings
- [ ] 4. AC4 direct read of a deleted agent surfaces an explicit deleted state (loud), not a silent not-found — named test asserts the exact response/MESSAGE
- [ ] 5. AC5 go build + go vet + go test green with fts5/CGO flags

## 2. Root cause & decisions

ROOT_CAUSE: Team-roster listing surfaces in internal/db/orgs.go (GetTeamMembers, GetTeamMemberNames, GetAllTeamMemberships, GetAgentTeams) joined team_members WITHOUT checking agents.status, so a soft-deleted agent (DeleteAgent sets status='deleted' and KEEPS both the agents row and the team_members row) kept leaking into every roster. Agent listings (ListAgents, GetAgentsByProfile, FindActiveAgentsBySkill, BuildOrgChart, web/projects) already excluded deleted via `status IN ('active','sleeping','inactive')` since 5a2791d — empirically list_agents(tsukumo) returns 0 of 280 deleted — so the residual leak was the roster join alone. Fix: add a conservative `NOT EXISTS (SELECT 1 FROM agents a WHERE a.name=<tm>.agent_name AND a.project=<tm>.project AND a.status='deleted')` guard to those four queries — filters only positively-deleted members, leaves the audit-trail row untouched and any bare/unregistered member unaffected. AC4 (loud direct read) needed no code: SenderEligibility already returns reason='deleted' (distinct from 'unregistered'); AC4 asserts that exact message via HandleIsEligible.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/orgs.go (4 roster queries), internal/db/registry_listing_deleted_test.go (new), internal/relay/is_eligible_deleted_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (full ./internal/... green)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- internal/db/orgs.go:GetTeamMemberNames — feeds team-broadcast delivery, so a soft-deleted agent now stops receiving team messages. Intended (consistent with delete_agent's "no more messages" contract), flagged in the plan for sign-off; plan-lane silence window (600s) elapsed with no veto.
- Guard is a correlated NOT EXISTS per row; team_members rosters are small and reads use the RO pool (d.ro()), so no hot-write-path or contention concern. CanMessage's team_members joins are permission checks (not listings) and are deliberately left unchanged — the send path already gates on SenderEligibility.

## 3. Files changed

```
internal/db/orgs.go                          |  23 ++-
 internal/db/registry_listing_deleted_test.go | 202 +++++++++++++++++++++++++++
 internal/relay/is_eligible_deleted_test.go   |  46 ++++++
 3 files changed, 268 insertions(+), 3 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `c877a9db-ffb5-4673-8ca3-dfc9504f379d`._
