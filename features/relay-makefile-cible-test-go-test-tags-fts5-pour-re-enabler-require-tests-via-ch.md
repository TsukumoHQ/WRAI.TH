# [relay] Makefile: cible `test:` (go test -tags fts5 ./...) pour re-enabler require_tests via check_kind=make

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/fts5-makefile (from main)
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
_(untyped ticket — no acceptance criteria)_

## 2. Root cause & decisions

ROOT_CAUSE: niwa gate's test_override() auto-detects `go test ./...` from go.mod, ignoring check_args, so it drops the required `-tags fts5` and every submit on agent-relay/WRAI.TH false-reds (sqlite FTS5 virtual table needs the CGO build tag). require_tests was disabled fleet-side as an interim workaround (memory: gate-test-override-ignores-check-args-fts5 / wraith-presubmit-missing-fts5-tag).

DECISION: add Makefile `test:` and `vet:` targets that run `go test -tags "fts5" ./...` / `go vet -tags "fts5" ./...`, mirroring the existing `build:` target's `$(GOFLAGS)`/tag convention. This lets cto flip `.niwa/config.json` to `check_kind=make` + `require_tests=true` — the gate then shells out to `make test` instead of auto-detecting, so the real fts5 suite runs.

REJECTED_ALTERNATIVE: patching agentd's test_override() to honor check_args directly — that's an agentd-side fix (ticketed a2c20b9d to backend-lead), out of this repo's lane and out of scope for this task.

Scope: Makefile only (+9/-1) — added `test:`/`vet:` targets + updated .PHONY. Did not touch .niwa/config.json (untracked, per task instruction — cto flips it after merge).

Verified: go build -tags fts5 ./... / go vet -tags fts5 ./... / go test -tags fts5 ./... all green. gofmt clean (no .go files touched).

## review-wraith verdict: SHIP
Scope: Makefile only
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK
BLOCKERS: none
NITS: none

## review-installation verdict: PASS
Scope: Makefile — test:/vet: targets only, no install.sh/install.ps1/service/checksum/sudo/PATH surface touched
BLOCKERS: none
NITS: none

## 3. Files changed

```
Makefile | 10 +++++++++-
 1 file changed, 9 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `9aa5467c`._
