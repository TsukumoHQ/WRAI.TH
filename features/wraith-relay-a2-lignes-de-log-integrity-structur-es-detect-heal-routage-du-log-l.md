# [wraith/relay] A2: lignes de log `integrity:` structurées detect+heal + routage du log live

## Team : wraith-backend (tsukumo)
## Branch : wraith/integrity-logs (from main)
## Relay task : 96a3021e-6399-4d85-b8ca-801f7d18cc97
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. une ligne `integrity: detect ...` par row quarantinée (class, table.col, ref value, row id) — test capture log
- [ ] 2. une ligne `integrity: heal ...` par résolution (class, row id, action, resolved_at) — test capture log
- [ ] 3. la cause du log figé est identifiée et écrite dans le report (fichier, rotation, redirection) avec le fix
- [ ] 4. après déploiement: au moins une ligne detect ET une ligne heal visibles dans le log live (chemin cité dans le report; si deploy hors ticket, commande de vérification fournie)
- [ ] 5. go test -tags fts5 ./internal/db/... ./internal/logging/... vert dans le worktree

## 2. Root cause & decisions

# A2: per-row `integrity:` detect/heal log lines (+ live-log routing report)

ROOT_CAUSE: The referential-integrity scan emitted only ONE aggregate line per pass (`integrity: <phase> referential scan — N open across M class(es): …`, referential_integrity.go:401). There was no per-row observability, so `grep integrity:` on the live relay log could not name a single quarantined row or a single healed one. The audit (afcd5a60) additionally reported the log itself as "frozen" — that reading was wrong (see below).

## The "frozen log" is a stale-file misread, not a routing bug (AC3)
Investigated the LIVE deployed relay directly:
- Process: pid 90992, `~/.local/bin/agent-relay serve`, started by launchd job `com.agent-relay`.
- `lsof -p 90992` → fds **1u and 2u both = `/Users/loic/.agentd/relay-serve.log`**. The launchd plist's `StandardOutPath`/`StandardErrorPath` both point there.
- That file is **fresh and growing** (6 MB, mtime today) and **already carries `integrity:` summary lines** (7 startup-scan summaries, counts 1445→1584).
- `~/.agent-relay/serve.log` (frozen Aug 19) is a **stale pre-launchd artifact** — nothing writes it anymore. This is the file the audit read as "frozen".
- `/tmp/agent-relay.log` (the value in the committed `com.agent-relay.plist` + `install.sh`) is **unused by the live job** (file does not exist); the running plist writes `relay-serve.log` instead.

Conclusion: slog → stderr → launchd `StandardErrorPath` → `relay-serve.log` works end to end. `internal/logging/logging.go` needs NO change (cto ruling, relay msg 20cce187). The only real gap was the missing per-row lines.

## What changed (2 files)
- `internal/db/referential_integrity.go`:
  - `runReferentialScan`: capture the transition delta BEFORE the per-class mutations — orphan rows not yet open (about to `detect`) and open rows no longer orphan (about to `heal`) — and emit the per-row lines AFTER `tx.Commit()` (a rolled-back scan logs nothing). Transition-only ⇒ a restart re-emits just the delta, so the initial ~1626-row scan logs once and steady-state is on-write only (bounded, per the ticket constraint).
  - `MarkQuarantine` (on-write soft-mark): one `integrity: detect …` line when it actually inserts (`RowsAffected>0`), silent on idempotent dedup.
  - Aggregate summary line kept unchanged.
- `internal/db/referential_integrity_test.go`: 5 tests (below).

Line formats:
- `integrity: detect class=<c> ref=<table.col> value=<v> row=<id>`
- `integrity: heal class=<c> row=<id> action=ref_resolved resolved_at=<ts>`

## Deploy verification (AC4)
Branch `wraith/integrity-logs`. After the next relay restart (deploy is out of this ticket's scope — CTO-owned), the per-row lines appear in the live log:
```
grep -E 'integrity: (detect|heal)' ~/.agentd/relay-serve.log
```
The first post-deploy startup scan re-detects the current open set once (~1.5k lines), then only the delta on subsequent scans and on-write marks.

REJECTED ALTERNATIVES:
- Re-log every open row each scan (simplest): unbounded — ~1.5k lines on EVERY restart (the relay restarts often), bloating an un-rotated launchd log. Rejected for the transition-delta approach.
- "Fix" `internal/logging/logging.go` routing/rotation as the ticket's premise suggested: routing is not broken (evidence above); a phantom fix would add risk to the fleet's log path for no gain. Rejected per cto ruling.
- SQLite `RETURNING` to get affected rows: driver/CGO sqlite version support is uncertain; a pre-mutation SELECT of the delta is portable and equally bounded.

```
## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/referential_integrity.go (per-row detect/heal + on-write MarkQuarantine detect), internal/db/referential_integrity_test.go (5 tests). logging.go untouched by ruling.
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (264 passed, db+logging)

Thesis check: no dropped messages / no double-dispatch / no second writer — the two capture SELECTs run inside the EXISTING scan writer-tx (same pattern as the count query at :300); per-row lines are emitted only AFTER commit, so a rolled-back scan logs nothing. Additive, backward-compatible, cannot red trunk.

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- referential_integrity.go: the scan now evaluates each class's orphanSQL 5×/pass (was 3× — 2 added capture SELECTs) on the 2-min sweeper. Bounded and infrequent; acceptable, flagged for awareness.
- The capture + string-build runs even when RELAY_LOG_LEVEL filters INFO (lines are dropped downstream at slog). Wasted work is small (infrequent scan); not guarded to avoid coupling internal/db to the logging level.
- First post-deploy scan holds the transition delta (~1.5k short strings) in memory before the post-commit flush; ~200 KB, documented as the bounded one-time cost.
```

## 3. Files changed

```
internal/db/referential_integrity.go      |  73 ++++++++++++++++-
 internal/db/referential_integrity_test.go | 128 ++++++++++++++++++++++++++++++
 2 files changed, 200 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `96a3021e-6399-4d85-b8ca-801f7d18cc97`._
