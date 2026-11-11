# [release CI] v1.26.0 release builds darwin/arm64: release.yml runner macos-14 (retired by GitHub) -> macos-15

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/d103a779-macos15-runner (from main)
## Relay task : d103a779-a55a-4f40-a3ce-fa9d78ba4068
## Trace : trace=959864981a0dcfbb945e8800227d8d82
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1: .github/workflows/release.yml contains no macos-14; the darwin/arm64 matrix entry runs on macos-15
- [ ] 2. AC2: diff touches only .github/workflows/*.yml (name-only diff pasted in submit note)
- [ ] 3. AC3: no CHANGELOG change; fix ships inside v1.26.0 (tag never released; cto-tsukumo moves the tag onto the merge commit)

## 2. Root cause & decisions

# d103a779 — release.yml darwin/arm64 runner macos-14 -> macos-15

ROOT_CAUSE: .github/workflows/release.yml:20 pinned the darwin/arm64 build to `macos-14`, which GitHub retired. Release run 43112239856 cancelled that job in 1s ("The configuration 'macos-14-arm64-us-default' is not supported"), so the release job was skipped and v1.26.0 has no assets.

FIX: `runner: macos-14` -> `runner: macos-15` on the darwin/arm64 matrix entry. macos-15 is the label the darwin/amd64 entry already builds green on.

OTHER RETIRED LABELS: grep of every `runs-on` / `runner:` in .github/workflows/ -> release.yml uses macos-15 + ubuntu-latest, announce-release.yml uses ubuntu-latest. Nothing else retired.

AC1: `grep -rn macos-14 .github/workflows/` -> no match; darwin/arm64 entry = macos-15.
AC2: git diff --name-only origin/main...HEAD
.github/workflows/release.yml
AC3: no CHANGELOG change.

CHECKS: YAML parses (ruby YAML.load_file), verify_cmd `go build -tags fts5 ./...` OK.
UNCERTAIN: the workflow itself only runs on a tag push, so the macos-15 arm64 build is proven by the release run after cto re-tags, not here.

## review-wraith verdict: SHIP
Scope: .github/workflows/release.yml (one runner label). No Go, schema, updater or install.sh change; REPO pin and the atomic-rename updater untouched.
Gate: build -tags fts5 OK / vet n/a / gofmt n/a (no .go touched) / test n/a (CI-config only)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

## 3. Files changed

```
.github/workflows/release.yml | 2 +-
 1 file changed, 1 insertion(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `d103a779-a55a-4f40-a3ce-fa9d78ba4068`._
