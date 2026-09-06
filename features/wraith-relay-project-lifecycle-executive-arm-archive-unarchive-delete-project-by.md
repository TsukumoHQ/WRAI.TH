# [wraith/relay] project-lifecycle executive arm: archive/unarchive/delete_project by an executive registered elsewhere — unblocks archiving the retired 'default' project

## Team : wraith-backend (tsukumo)
## Branch : fix/project-lifecycle-exec-arm (from main)
## Relay task : 05936947-cfad-4509-9860-527d35157a03
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: an is_executive agent registered only in project A calls archive_project project=B (no registration in B, B may be 'default') -> result archived:true, and an audit_log row Action='project.lifecycle.admin' names actor + target project + home project; revert-check: removing the executive arm makes the test fail with the 'is not registered in project' refusal
- [ ] 2. AC2 named test: a NON-executive agent registered only in A calling archive_project project=B is refused with the existing 'is not registered in project' text — refusal text unchanged
- [ ] 3. AC3 named test: caller registered in the target project archives as today (regression guard, path unchanged); existing archived_project_test.go + projects_archive_test.go stay green
- [ ] 4. AC4 named test: unarchive_project and delete_project take the same arm (delete_project on an archived target by a remote executive succeeds; on a non-archived target still refuses 'archive_project it first')
- [ ] 5. AC5 scope: diff touches internal/relay/toolset.go + one test file only; register_agent anonymous/default refusal and refuseIfArchived untouched

## 2. Root cause & decisions

# [wraith/relay] project-lifecycle executive arm

Task: 05936947-cfad-4509-9860-527d35157a03

ROOT_CAUSE: `archive_project`/`unarchive_project`/`delete_project` are gated by `guardIdentity`, which resolves the project from the SAME `project` arg the tool uses as its TARGET, then requires the caller to be registered in that project (toolset.go, "agent %q is not registered in project %q"). The retired 'default' catch-all refuses registration by design (`anonymousRefusedError`), so no one can ever be registered there — making 'default' (and any zero-agent shell) permanently unarchivable. Deadlock: the only tool that could retire the project cannot be called against it by anyone.

DECISION: Add a narrow executive arm at the `agent == nil` seam in `guardIdentity`. `projectLifecycleTools = {archive_project, unarchive_project, delete_project}`. When the tool is one of these AND the caller is not registered in the target project, `callerIsActiveExecutive(from)` looks the caller up across projects (reusing `ProjectsOfAgent` + `GetAgent`, both read-pool; no new query, no wide-table column touched); if ANY row for `from` is `status='active'` and `is_executive`, the call is allowed and a `RecordAudit{Action:'project.lifecycle.admin', Actor:from, Project:targetProject, Reason:'executive arm from project <home>'}` row is written, then it flows to the handler. Every other unregistered caller — including a non-executive — falls through to today's unchanged refusal. A caller registered in the target project never takes the new path (byte-identical behavior). `create_project` is intentionally absent (bootstrap tool, never wrapped). `anonymousRefusedError` and `refuseIfArchived` untouched, per the founder directive (no anonymous agents) and DEC-wraith-archive-project-1.

Scope: `internal/relay/toolset.go` + one new test file `internal/relay/project_lifecycle_exec_test.go`.

Verification: `go build -tags fts5 ./...` OK, `go vet -tags fts5 ./internal/relay` OK, `gofmt -l` clean, `go test -tags fts5 ./...` green (rebased onto current main, full suite green).
- AC1: TestLifecycleArm_RemoteExecutiveArchives — exec in 'home' archives 'target-b' (unregistered there), audit row names actor+target+home. TestLifecycleArm_RemoteExecutiveArchivesDefault — target may be 'default'.
- AC1 revert-check: neutering the `projectLifecycleTools` branch (`if false && …`) turns TestLifecycleArm_RemoteExecutiveArchives red with the exact "is not registered in project" refusal; restored → green.
- AC2: TestLifecycleArm_NonExecutiveRefused — non-exec refused with unchanged text, no audit row.
- AC3: TestLifecycleArm_RegisteredCallerUnchanged — caller registered in target archives as today, arm does not fire (no arm audit); existing archived_project_test.go stays green.
- AC4: TestLifecycleArm_UnarchiveAndDelete — remote exec unarchives; delete purges an archived target but still refuses a non-archived one ("archive_project it first", enforced by the handler after the guard passes).

Out of scope: the actual archive of 'default' (cto-tsukumo runs it after redeploy).

GOTCHA recorded: a Go test file named `*_arm_test.go` is silently EXCLUDED from the build on non-arm GOARCH — go reads the `_arm` segment as an implicit `GOARCH=arm` build constraint (we build arm64). The file's tests never compile and `go test -run` reports "no tests to run" with no error. Original name `project_lifecycle_arm_test.go` hit this; renamed to `project_lifecycle_exec_test.go`. Avoid GOOS/GOARCH tokens (arm, amd64, arm64, linux, darwin, windows, …) as trailing filename segments before `_test.go`.

Rejected alternatives:
- New DB method `FindExecutiveAgentByName` in internal/db/agents.go: would push the diff to a 3rd file (triggering [split]) for no gain — `ProjectsOfAgent` + `GetAgent` already give the cross-project-by-name lookup using the read pool.
- Exempting 'default' from anonymousRefusedError so an agent could register there and archive it: rejected — founder directive is no anonymous/default registrations; the arm keeps that refusal intact.
- Checking is_executive in the handler instead of the guard: the guard is the single seam that already refuses unregistered callers; the handler has no identity context and adding it there duplicates resolution.

Security note (for the reviewer): the arm skips the best-effort session-binding anti-theft check (it early-returns `next`), but that check only proves impersonation when the connection is bound to a different agent IN THE TARGET project — a cross-project caller is never bound there, so the check is inapplicable and skipping it changes nothing. The arm trusts the `as` identity exactly as the existing model already does for every registered call (session binding is documented fail-open). No new attack-surface class; the arm is is_executive-gated and audited.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/relay/toolset.go (guardIdentity seam + two helpers) + internal/relay/project_lifecycle_exec_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (full ./... green after rebase onto current main)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- The arm early-returns `next(ctx, req)`, skipping the session-binding anti-theft check for lifecycle tools. As analysed above this is inapplicable cross-project (the caller is never session-bound in the target project), so behaviour is unchanged — noted only so the reviewer sees the skip is deliberate, not an oversight.
- callerIsActiveExecutive is O(projects-of-caller) read-pool lookups; lifecycle tools are rare admin ops so this is not a hot path. No caching added on purpose (staleness would be worse than a few RO reads).

Invariants checked: single-writer intact (only the existing best-effort RecordAudit writer path is used; lookups are read-pool). agentColumns/scanAgent untouched. No schema/migration. refuseIfArchived + anonymousRefusedError byte-identical. No new MCP tool; registry unchanged. Existing archived_project_test.go green.

## 3. Files changed

```
...tive-arm-archive-unarchive-delete-project-by.md |  77 +++++++++
 internal/relay/project_lifecycle_exec_test.go      | 180 +++++++++++++++++++++
 internal/relay/toolset.go                          |  52 ++++++
 3 files changed, 309 insertions(+)
```

## 4. QA Log

### Round 1 — ✅ APPROVED by review-05936947-cfad-4509-9860-527d35157a03 @ `562342a89`
- 🟢 AC1: AC1 audit row inspected via lifecycleAdminAudit helper, asserts actor=chief, project=target-b, reason contains home — evidence: internal/relay/toolset.go:445-455 adds projectLifecycleTools arm; testLogsTestLifecycleArm_RemoteExecutiveArchivesDefault PASS in 0.02s; revert-check: neutering `if projectLifecycleTools` to `if false && projectLifecycleTools` in /tmp copy turns the test red with exact `agent "chief" is not registered in project "target-b" — call register_agent first.` — test: TestLifecycleArm_RemoteExecutiveArchives + TestLifecycleArm_RemoteExecutiveArchivesDefault internal/relay/project_lifecycle_exec_test.go:38-89
- 🟢 AC2: non-executive (worker) refused exactly as before, no arm audit, no archive side-effect — evidence: internal/relay/toolset.go:456 unchanged refusal text `agent %q is not registered in project %q — call register_agent first.`; expectNotRegistered helper asserts containment; archive NOT called (IsProjectArchived false) and zero arm audit rows — test: TestLifecycleArm_NonExecutiveRefused internal/relay/project_lifecycle_exec_test.go:91-105
- 🟢 AC3: regression guard green; registered-caller path byte-identical (arm never fires) — evidence: TestLifecycleArm_RegisteredCallerUnchanged: caller registered in target takes agent!=nil path, archives successfully, zero arm audit rows; existing TestArchiveTaskRESTSuccessThenConflict + TestArchivedProjectRefusesRegisterDispatchSendClaim + TestArchiveBoard* + TestArchiveTasks all PASS pre-existing on main and still PASS in branch — test: TestLifecycleArm_RegisteredCallerUnchanged internal/relay/project_lifecycle_exec_test.go:108-124 + existing TestArchiveTaskREST* / TestArchivedProject* / TestArchiveBoard* / TestArchiveTasks internal/relay
- 🟢 AC4: unarchive+delete take same arm; delete handler still enforces archived-first — evidence: TestLifecycleArm_UnarchiveAndDelete verifies all three: remote exec unarchives target-u (was archived), deletes archived target-d (purges), refuses delete on non-archived target-active with exact `archive_project it first` text — test: TestLifecycleArm_UnarchiveAndDelete internal/relay/project_lifecycle_exec_test.go:127-178
- 🟢 AC5: scope contained; refusal paths byte-identical — evidence: git diff origin/main..fix/project-lifecycle-exec-arm --name-only: only internal/relay/toolset.go + internal/relay/project_lifecycle_exec_test.go (plus .niwa-decision.md + features/*.md scribe artifacts, non-code); grep confirms anonymousRefusedError at line 36 and refuseIfArchived at line 113/418 unchanged vs origin/main; TestRegisterRefusesAnonymousAndDefault PASS + all TestArchivedProject* PASS — test: TestRegisterRefusesAnonymousAndDefault + TestArchivedProject* internal/relay

## 5. Timeline

- round 1 → **approve** (review-05936947-cfad-4509-9860-527d35157a03)

**Approve-with-findings (follow-up):** go test -tags fts5 ./... 841 ok; TestLifecycleArm 5/5 ok; revert-check (arm neutered in /tmp copy) makes TestLifecycleArm_RemoteExecutiveArchives fail with exact 'is not registered in project' refusal; TestRegisterRefusesAnonymousAndDefault + archived-project suite green; scope toolset.go + 1 test file; anonymousRefusedError+refuseIfArchived untouched

---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `05936947-cfad-4509-9860-527d35157a03`._
