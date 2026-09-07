# [split][wraith/ext-triage] dependabot #147 (go-deps group) + #146 (actions/setup-go 6->7) as ONE bump — rebase, verify, gate

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/ext-deps (from main)
## Relay task : ad5035ab-61f2-4a6e-9e2e-3d8d9ef70a14
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestDepsBumpBuildGreen: go build -tags fts5 ./... and go test -tags fts5 ./... green with the bumped go.mod/go.sum; go mod tidy leaves no diff
- [ ] 2. AC2 TestWorkflowsSetupGoV7: source-scan test asserts every .github/workflows/*.yml that uses actions/setup-go pins @v7 and none still references @v6; no other workflow line changed beyond what #146 carried plus the input fix if required

## 2. Root cause & decisions

# Decision — dependabot bump #147 + #146 (task ad5035ab)

## ROOT_CAUSE
Two external-dependency PRs left open by dependabot, to land as one bump:
#147 (go-deps group: mark3labs/mcp-go, mattn/go-sqlite3) and #146 (actions/setup-go 6->7).

## CHANGE (4 files vs main fecd12b)
- go.mod / go.sum: cherry-picked #147 (179a77d) — mark3labs/mcp-go v0.55.1 -> v1.0.0,
  mattn/go-sqlite3 v1.14.47 -> v1.14.50. `go mod tidy` leaves no diff. mcp-go v1.0.0
  is a v0->v1 major but keeps the same import path and compiles clean — NO source change.
- .github/workflows/release.yml: cherry-picked #146 (29b8eead) — actions/setup-go @v6 -> @v7.
- deps_bump_test.go (new, package main): AC1 + AC2 source-scan tests.

## CONFLICT RESOLUTION (scope note)
#146 originally touched 4 workflows (lint, release, test-install, test). origin/main
has since DELETED lint.yml, test-install.yml, test.yml, so only release.yml survives
and carries the @v7 bump; the other three stay deleted (not resurrected). Net diff is
therefore 1 workflow file, not 4 — a consequence of main's workflow reorg, not a scope cut.

## #146 CHECKS
The only failing check on #146 is "review-skill verdict present" — the gate check that
greps the PR body for a review verdict block, which a raw dependabot PR has none of.
Every real CI check (Go tests, Linux/Windows/macOS build matrix, golangci-lint) is GREEN.
So the "setup-go@v7 breaking change" the ticket anticipated did NOT occur — no input fix needed.

## ACs
- AC1 TestDepsBumpBuildGreen: go.mod pins mcp-go v1.0.0 + go-sqlite3 v1.14.50;
  `go build -tags fts5 ./...` + `go test -tags fts5 ./...` green (894); tidy no-diff.
- AC2 TestWorkflowsSetupGoV7: every .github/workflows/*.yml on setup-go pins @v7, no @v6.
- diff = exactly the 4 files above (rebased onto main fecd12b).

## review-wraith verdict: SHIP
- Build bar: fts5/CGO build + full suite green, 894 pass across 12 packages; tidy no-diff.
- No schema, no migration, no query, no lock, no handler change — dependency versions only.
- mcp-go major bump compiles with zero API breakage in this tree (verified by the build).
- Dependabot authorship preserved on both picked commits.
- Blast radius: module graph + one CI action pin; no runtime code touched.

VERDICT: SHIP.

## 3. Files changed

```
.github/workflows/release.yml |  2 +-
 deps_bump_test.go             | 44 +++++++++++++++++++++++++++++++++++++++++++
 go.mod                        |  4 ++--
 go.sum                        | 10 ++++++----
 4 files changed, 53 insertions(+), 7 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `ad5035ab-61f2-4a6e-9e2e-3d8d9ef70a14`._
