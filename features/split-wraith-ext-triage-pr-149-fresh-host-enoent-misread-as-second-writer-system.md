# [split][wraith/ext-triage] PR #149 fresh-host ENOENT misread as second writer; systemd-less installs die post-binary — rebase on main, verify, gate

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/ext-149 (from main)
## Relay task : 1650f8a0-c786-42e3-b3ca-f358274218f9
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 TestServeLockFreshHostENOENT: on a path whose parent directory does not exist, acquiring the serve lock succeeds (creates it) instead of reporting a second writer; a second acquire on the same path is still refused
- [ ] 2. AC2 TestServeLockDiffScope: go build -tags fts5 ./... and go test -tags fts5 ./... green on the rebased branch; diff touches only install.sh main.go serve_lock_unix.go serve_lock_windows.go (+ the AC1 test file); original author kept as commit author

## 2. Root cause & decisions

# T1650f8a0 — ext-triage PR #149: fresh-host serve-lock ENOENT

## Root cause
`acquireServeLock` opened `<db>.lock` without creating `~/.agent-relay` first,
and `main` treated EVERY lock error as "another relay is already serving". On a
fresh host (container, first install) the state dir doesn't exist yet → ENOENT
misread as a second writer → serve died before `db.New` ever created the dir.
Systemd-less installs also died: an unguarded `systemctl` under `set -e` killed
the install AFTER the binary landed.

## Decision
Rebase external PR #149 (author helios-code, commit 8a18b2a) onto main d35ab8c,
keeping the author's fix + authorship; resolve the one main.go conflict; add the
missing regression test.
- serve_lock_unix.go: `MkdirAll` the lock's parent; return sentinel `errLockHeld`
  ONLY on EWOULDBLOCK/EAGAIN — a real concurrent holder still refused. Every other
  error surfaces as itself.
- serve_lock_windows.go: mirror the sentinel so `errors.Is` compiles.
- main.go (conflict resolved): keep the PR's errLockHeld distinction, expressed in
  main's current slog+os.Exit(1) style (main had moved off log.Fatalf).
- install.sh: `install_systemd_service` warns + `return 0` when systemctl is absent
  or the user bus is down, instead of dying under `set -e`.

## Single-writer guarantee (review-wraith thesis)
Preserved. Two relays on one path: A holds flock, B's `LOCK_EX|LOCK_NB` →
EWOULDBLOCK → errLockHeld → `os.Exit(1)`. MkdirAll only ensures the dir exists;
the flock still gates. Test asserts BOTH directions.

## ACs → tests
- AC1 fresh-host ENOENT acquire succeeds + second acquire refused → TestServeLockFreshHostENOENT
- AC2 build+test green, diff scope = 4 PR files + 1 test, author preserved → the verify + git log

## Rejected alternatives
- Drop the PR / re-author: loses external contributor authorship; task says keep it.
- Tolerate all lock errors: would weaken single-writer — rejected; sentinel is EWOULDBLOCK/EAGAIN-only.

## review-wraith verdict: SHIP
Scope: install.sh, main.go, serve_lock_unix.go, serve_lock_windows.go, NEW serve_lock_unix_test.go
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (all packages)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- Windows serve is a documented no-op (not a relay host in this deployment); unchanged by this PR.

## 3. Files changed

```
install.sh              | 16 ++++++++++-
 main.go                 | 12 ++++++--
 serve_lock_unix.go      | 21 ++++++++++++--
 serve_lock_unix_test.go | 73 +++++++++++++++++++++++++++++++++++++++++++++++++
 serve_lock_windows.go   |  6 ++++
 5 files changed, 123 insertions(+), 5 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1650f8a0-c786-42e3-b3ca-f358274218f9`._
