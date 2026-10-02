# [relay/wedge] relay MCP + REST hang (>20s, every DB-backed call) while / answers in 0.3s: 65 ESTABLISHED + 61 CLOSE_WAIT on :8090; needed a launchd restart

## Team : wraith-backend (tsukumo)
## Branch : wraith/55323073-relay-wedge (from main)
## Relay task : 55323073-e221-4066-83f7-e62ec5982078
## Trace : trace=7bdd8c98530498a1b20574d2cf26a610
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. root cause named with file:line from the dump
- [ ] 2. test: a handler blocked on the DB returns an error within the request deadline instead of hanging (fake slow writer)
- [ ] 3. test: burst of 200 concurrent RecordHookEvent + task reads completes; no handler exceeds the deadline
- [ ] 4. /healthz (or /readyz) does a cheap DB read with a 2s timeout and returns 503 when it fails (test)
- [ ] 5. go test -tags fts5 ./... green

## 2. Root cause & decisions

# 55323073 relay wedge: DB reads fail fast, Detector resolves outside its lock, /healthz probes the DB

ROOT_CAUSE: reader-pool starvation with no deadline, not a leaked lock. Full SIGQUIT dumps (in /tmp/agent-relay.err; the ~/.agentd/field copies are truncated): all 10 reader conns (db.go:139 SetMaxOpenConns(10)) in sqlite3_step at internal/db/projects.go:417 (ListProjectsWithInfoFiltered, polled by /api/projects; aggregates all tasks + ~187k token_usage rows/24h via a non-covering index) while the host was IO-stalled (load 140, statusline.sh in U state). d.ro() reads had no context, so ~170 handlers parked forever in database/sql conn wait (sql.go:1369): ListAgents, IsProjectArchived, ListTasks, ResolveTaskID, GetAgentBySessionID. The Detector.mu holder the ticket could not find is G19412: detector.go:168 (d.resolve under d.mu) -> main.go:200 -> GetAgentBySessionID agents.go:339, itself in the conn wait; 284 (18:40) / 117 (18:56) hook handlers queued at detector.go:155 behind it.

FIX: (1) db.go: ro() returns roPool; Query/QueryRow run under readerTimeout=10s (pool wait + statement; go-sqlite3 interrupts on cancel; QueryContext callers keep their own ctx). Side effect: a Rows/Row a caller forgets to close now frees its conn at the deadline. (2) projects.go: concurrent ListProjectsWithInfoFiltered share one query (singleflight, no cache, zero staleness). (3) detector.go: RecordEvent resolves outside d.mu (RLock peek, resolve, Lock + set if still unbound). (4) GET /healthz (mux root, behind the full middleware chain, outside the /api Host guard): reader read + writer checkout, 2s each, 503 + per-pool error on failure. (5) db/testing.go: HoldPoolsForTest, Set{Reader,Writer}TimeoutForTest.

REJECTED: TTL cache on the projects stats (stale counts after writes, breaks tests); exporting a per-request ctx through every handler (286 ro() call sites; the pool deadline gives the same bound with no signature churn); covering index on token_usage (schema change on a 312MB analytics DB at boot, separate ticket if still needed).

FOLLOW-UPS: niwa doctor should poll /healthz and act on N consecutive 503s (a single long writer tx can trip the writer probe); 9 unbounded d.conn reads (skills.go:20/64, profiles.go:31, messages.go:551) still wait on the writer without a deadline; field dump capture truncates at ~1k lines (full dump is in the relay stderr log).

CROSS-LANE: internal/ingest/detector.go is wraith-engine's zone; change named in the ticket, FYI sent.

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/{db,projects,testing}.go, internal/ingest/detector.go, internal/relay/{api,relay}.go + tests
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 ./... OK / -race db+ingest+relay OK

BLOCKERS (must fix before merge):
- none. Single writer untouched (SetMaxOpenConns(1), no new writes); no schema/migration; no hot-path write; activity stays in-memory; middleware order intact (/healthz behind CORS->RateLimit->BodyLimit->Auth).

NITS (non-blocking):
- db.go readerTimeout: a d.ro().Query whose iteration exceeds 10s is now cut short (rows.Err = deadline). Sampled loops (ReleaseReadyHolds, SyncFounderGates, commitRollouts) collect then close before writing, so not affected.
- /healthz is a root mux route, not a ServeAPI case: on purpose, so a probe from any host is never turned into a 421 by the /api Host guard.

RED_EVIDENCE:
cmd: go test -tags fts5 -count=1 -run 'SlowResolve|BurstWithSlow' ./internal/ingest/
test_sha: f215ebe
output:
--- FAIL: TestDetector_SlowResolveDoesNotHoldLock
    detector_test.go:128: a stuck resolve blocked an unrelated RecordEvent/GetSessions — d.mu is held across the DB read
--- FAIL: TestDetector_BurstWithSlowResolver
    detector_test.go:155: 200-event burst took 10.617142541s — resolves are serialized under d.mu
FAIL	agent-relay/internal/ingest
also (reader pool, same-tree check with roPool reverted to plain Query/QueryRow):
    reader_wedge_test.go:47: reader call hung well past readerTimeout — pool-wait is not bounded

## 3. Files changed

```
internal/db/db.go                |  81 +++++++++++++++-
 internal/db/projects.go          |  45 +++++++++
 internal/db/reader_wedge_test.go | 143 ++++++++++++++++++++++++++++
 internal/db/testing.go           |  50 ++++++++++
 internal/ingest/detector.go      |  30 ++++--
 internal/ingest/detector_test.go |  70 ++++++++++++++
 internal/relay/api.go            |  26 ++++++
 internal/relay/relay.go          |   4 +
 internal/relay/wedge_test.go     | 195 +++++++++++++++++++++++++++++++++++++++
 9 files changed, 633 insertions(+), 11 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `55323073-e221-4066-83f7-e62ec5982078`._
