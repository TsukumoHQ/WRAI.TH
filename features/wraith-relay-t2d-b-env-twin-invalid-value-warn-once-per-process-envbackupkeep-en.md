# [wraith/relay] T2d-b env-twin invalid-value WARN once per process: envBackupKeep/envReviewerTTL (cleanup.go:84/:119) now log on every 5-minute tick when RELAY_BACKUP_KEEP / RELAY_REVIEWER_TTL_DAYS is set but invalid

## Team : wraith-backend-2 (tsukumo)
## Branch : wraith-backend-2/t2d-b (from main)
## Relay task : 65380b0b-a6ee-4087-a9e3-018d0269b478
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: with RELAY_BACKUP_KEEP=abc and stored backup_keep=9, three consecutive tickBackupKeep(database) calls return 9 and the captured log contains the invalid line exactly once; same for RELAY_REVIEWER_TTL_DAYS=abc / reviewer_ttl_days with tickReviewerTTL; with a valid env the log stays empty and the env value wins
- [ ] 2. AC2 named smoke test: TestResolveBackupKeep, TestResolveReviewerTTL and the 5 T2b tests still green; scope: diff touches exactly internal/relay/cleanup.go, internal/relay/cleanup_test.go; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

# T2d-b — env-twin invalid-value WARN once per process

## ROOT_CAUSE
T2b ruling A7 moved the env probes `envBackupKeep()` / `envReviewerTTL()`
(cleanup.go) into the cleanup tick, which runs every `PurgeInterval` (5 min).
Their bare `log.Printf("RELAY_BACKUP_KEEP=%q invalid; using default …")` therefore
re-logged on every tick whenever `RELAY_BACKUP_KEEP` / `RELAY_REVIEWER_TTL_DAYS`
was set but unparsable — log spam. My own T2b gate finding (msg 3c5f6305).

## CHANGE (2 files, behaviour change = WARN dedupe only)
- `internal/relay/cleanup.go`: add package-level `var envWarnedKeys sync.Map`
  (mirrors `db.settingWarnedKeys`, projects.go) + `warnEnvOnce(name, format, args…)`
  that `LoadOrStore`s on the env name and `log.Printf`s only on first sight; add the
  `sync` import; the two invalid-path `log.Printf` calls in `envBackupKeep` /
  `envReviewerTTL` now go through `warnEnvOnce`. Return values (`ok=false` → stored
  setting, else const) and the env>stored>const precedence are unchanged.
- `internal/relay/cleanup_test.go`: NEW AC1 `TestEnvTwinInvalidValueWarnsOncePerProcess`
  beside the T2b tests — resets the two `envWarnedKeys` entries for order-independence,
  then asserts exactly one WARN line each across three `tickBackupKeep`/`tickReviewerTTL`
  reads under an invalid env (returns stay 9 = stored), and empty log + env wins on a
  valid env.

`cleanup_backup_test.go` untouched (`TestResolveBackupKeep`/`TestResolveReviewerTTL`
assert return values only, still green). Dedup keys on the env NAME per the ticket
("LoadOrStore on the env name") — goal is one line per process, satisfied.

## review-wraith verdict: SHIP
Scope: internal/relay/cleanup.go, cleanup_test.go (log-dedupe in the cleanup tick's env probes).
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (477 pass, +1 new test).

BLOCKERS (must fix before merge):
- none.

NITS (non-blocking):
- none.

Not touched: sqlite writer/lock, schema/migrations, task transitions, deliveries/inbox,
auth/middleware, MCP registry, ingest/SSE, updater/release. `envWarnedKeys` is a
`sync.Map` (concurrency-safe); the tick is single-goroutine anyway.

## 3. Files changed

```
internal/relay/cleanup.go      | 21 +++++++++++++++--
 internal/relay/cleanup_test.go | 52 ++++++++++++++++++++++++++++++++++++++++++
 2 files changed, 71 insertions(+), 2 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `65380b0b-a6ee-4087-a9e3-018d0269b478`._
