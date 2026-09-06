# [wraith/relay][split] GH billed-CI OFF per founder order (cto directive 074de014): remove PR/push test+lint workflows, keep release family

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/ci-off (from main)
## Relay task : 3a3a3c16-80eb-4197-90ba-83ee8af0e819
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 lint.yml, test.yml, skill-gate.yml, test-install.yml deleted; release.yml + announce-release.yml byte-identical
- [ ] 2. AC2 go test -tags fts5 ./... green locally (nothing depended on workflow files)
- [ ] 3. AC3 diff touches ONLY the 4 deleted workflow files

## 2. Root cause & decisions

ROOT_CAUSE: billed GitHub Actions (PR/push-triggered lint.yml, test.yml, skill-gate.yml, test-install.yml) run on every PR/push at cost; founder ordered them off (cto directive 074de014, relayed 23:51Z). The quality bar already lives fully in the niwa gate — local presubmit `go build/vet/test -tags fts5 ./...` plus the niwa/qa-gate App check-run, neither of which is billed Actions — so these four workflows are pure redundant spend. lint.yml is also the golangci-lint job that made PR #167 red. Release-family workflows (release.yml, announce-release.yml) are tag-triggered, not PR/push, and founder decides them separately, so they stay untouched.
FIX: delete the four PR/push-triggered workflow files; nothing in the build/test depends on them. Diff is four deletions only.

## review-wraith verdict: SHIP
Scope: 4 pure deletions of PR/push-triggered GitHub Actions workflows (.github/workflows/lint.yml, test.yml, skill-gate.yml, test-install.yml); release.yml + announce-release.yml byte-identical; no Go source, schema, handler, or updater touched.
Gate: build -tags fts5 OK / vet OK / gofmt N/A (no .go) / test -tags fts5 OK (808 passed, 12 pkgs)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none — pure CI-cost removal per founder order; skill-gate verdict grep now folds into the qa-submit presubmit, so deleting skill-gate.yml removes duplicate billed work, not coverage.

## 3. Files changed

```
.github/workflows/lint.yml         |  25 -------
 .github/workflows/skill-gate.yml   |  71 -------------------
 .github/workflows/test-install.yml | 141 -------------------------------------
 .github/workflows/test.yml         |  35 ---------
 4 files changed, 272 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `3a3a3c16-80eb-4197-90ba-83ee8af0e819`._
