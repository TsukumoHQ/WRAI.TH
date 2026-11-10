# [relay/identity] a host stall of a few seconds never fails an authed call, and the relay stops adding its own multi-second integrity scans to the load

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/33dab33d-identity-deadline (from main)
## Relay task : 33dab33d-a093-49bf-b7de-e68b34d9ead2
## Trace : trace=73df49e5e80afdbe5c1eded138e7f98e
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. B1: the identity gate retries GetAgent once on a deadline error; a test with a GetAgent that times out once then succeeds proves the authed call succeeds (red without the retry)
- [ ] 2. B2: a short-TTL (<= 5s) per-agent identity cache serves a repeat identity check without a DB read; a test proves a revoked/deactivated agent is refused within the TTL bound (cache never extends access beyond TTL)
- [ ] 3. B3: the writer logs the HOLDER (op name + hold ms) when a write holds the lock > 2s; a test asserts the log line
- [ ] 4. C: the integrity scan (orphan_* queries) costs measurably less: before/after duration of each scan query on a copy of the live DB in the PR body, and no single scan query > 2s on that copy
- [ ] 5. go test -tags fts5 ./... green
- [ ] 6. PR body names the root cause as measured (host saturation + writer queue + scan cost) with file:line for B and C

## 2. Root cause & decisions

# 33dab33d — identity-check deadline (rescoped by cto-tsukumo 13:29Z: B + C; A = host capacity, founder)

## Root cause (measured)
ROOT_CAUSE: host saturation (load 55 / 18 cores, swap 10.7/12GB) stalls the relay past readerTimeout; the single-writer queue then backs up (15-23s waits), and the relay adds its own load via a 2-min referential scan that full-scans the wide tasks table 8 times (~94MB of page reads) which, evicted under memory pressure, re-reads from disk every pass. Not a write lock blocking identity reads (refuted by probe).
Host saturation + writer queue + integrity-scan I/O, not a write lock blocking identity reads.
- GetAgent = WAL reader pool (internal/db/agents.go:318, readerTimeout 10s internal/db/db.go:184). Probe TestGetAgent_NotBlockedByHeldWriteLock (internal/db/identity_read_lock_test.go) GREEN on origin/main dba9a15 -> a held write lock does not block the identity read.
- Live log /private/tmp/agent-relay.err: in the windows 11-09 10:59-11:03 and 11-10 14:21-14:24 local, reads (get agent by session, sweeps) AND writes (writer wait 15-23s on UPDATE agents SET last_seen) hit the deadline together = host stall.
- Host 13:28Z: loadavg 55 (5m) / 18 cores, swap 10.7/12GB, ~140MB free.
- Integrity scan: 1,411 "integrity scan: took" lines >=2s, read phase 2-29s, slowest class = orphan_dispatcher (first class, cold cache) in recent lines.

## B (d22ef3a)
- B1 retry once on DeadlineExceeded: internal/relay/identity_cache.go:99 (h.identityAgent, called from guardIdentity internal/relay/toolset.go).
- B2 identity cache TTL 3s, eligible rows only, invalidated on deactivate/delete: internal/relay/identity_cache.go (identityCacheTTL), handlers_agents.go cascadeDeactivatedAgent.
- B3 writer HOLDER log >2s: internal/db/db.go writerHoldSlow/logWriterHold, writerTx.release.

## C (fe798dd) — integrity scan cost, copy of live relay.db (sqlite3 .backup, 97.5MB, tasks 3,543 rows / 12.4MB)
Cause: 8 tasks-table classes each full SCAN tasks (wide rows) -> ~2.9k pages each, ~24k pages per scan every 2 min; evicted under memory pressure -> re-read from disk.
Fix: idx_tasks_refscan (partial archived_at IS NULL, covering) internal/db/db.go:1250-1262. Plan pinned by internal/db/referential_scan_plan_test.go.

Per class, fresh sqlite3 connection (pages read = sqlite page-cache misses; ms = best of 3), before -> after:
| class | rows | pages before | pages after | ms before | ms after |
| orphan_dispatcher | 84 | 2878 | 92 | 16 | 3 |
| orphan_assignee | 82 | 2851 | 93 | 14 | 3 |
| orphan_claimer | 65 | 2878 | 93 | 11 | 3 |
| limbo | 22 | 3129 | 100 | 11 | 2 |
| orphan_profile | 4 | 2942 | 192 | 16 | 8 |
| orphan_parent | 0 | 2842 | 117 | 11 | 2 |
| orphan_board | 3 | 3151 | 86 | 11 | 3 |
| orphan_task_project | 0 | 3186 | 86 | 11 | 3 |
| other 8 classes (unchanged) | - | 1831 | 1831 | 2-6 | 2-4 |
| TOTAL | | 25688 (~100MB) | 2690 (~10.5MB) | | |
Real readRefDeltas (Go, reader DSN) on the copy: cold read phase 486ms -> 25ms; warm 74-87ms -> 23-24ms. Max single query on the copy: 16ms before, 8ms after (AC: <2s). Orphan row counts identical per class.
Honest note: on an idle host every query was already <2s; the field seconds are this I/O under swap. The fix cuts the pages the scan pulls per pass by 9.5x (tasks classes 28x).
Plan stable after ANALYZE (PRAGMA optimize in prod). Write cost: one extra index on tasks (~65-90 pages).

Follow-up (not in this diff, UNCERTAIN): reader pool PRAGMAs mmap_size/cache_size (internal/db/db.go:141-143) run via reader.Exec, so they apply to one pooled connection only. Not changed: raising per-connection cache on 10 conns would add memory on a host already in swap.

## Verify
go test -tags fts5 ./... -> 1551 passed, 12 packages (fe798dd).

RED_EVIDENCE:
  cmd: go test -tags fts5 ./internal/relay/ -run TestGuardIdentity_RetriesOnceOnDeadline -count=1   (retry condition at identity_cache.go:97 on d22ef3a disabled)
  test_sha: d22ef3a
  output: |
    --- FAIL: TestGuardIdentity_RetriesOnceOnDeadline (0.12s)
        identity_cache_test.go:49: one stalled identity read must not fail the call (calls=1): [{{<nil>} <nil> text {"code":"INTERNAL","errorCategory":"transient","isRetryable":true,"message":"identity ch
    FAIL
    FAIL	agent-relay/internal/relay	0.794s
  also (C, test_sha fe798dd, idx_tasks_refscan creation removed): go test -tags fts5 ./internal/db/ -run TestRefScan_TasksClassesUseCoveringIndex
    referential_scan_plan_test.go:65: fresh: class orphan_dispatcher outer tasks access = "SCAN t", want covering idx_tasks_refscan
    FAIL

## review-wraith verdict: SHIP-WITH-NITS
Scope: internal/db/db.go (writerExec/writerTx hold log, Optimize, idx_tasks_refscan migration), internal/relay/identity_cache.go + toolset.go guardIdentity + handlers_agents.go invalidate, tests (identity_cache_test, db_writerwait_test, identity_read_lock_test, referential_scan_plan_test)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (1551 passed, 12 packages)

BLOCKERS (must fix before merge):
- none. Single writer intact (writerExec checks out the same SetMaxOpenConns(1) conn); migration additive + idempotent (CREATE INDEX IF NOT EXISTS, all columns ensured earlier in migrate); no agentColumns/scanAgent change; no new hot-path write; cache map mutex-guarded; cache only grants (refusals never cached), TTL 3s never extended by a hit.

NITS (non-blocking):
- internal/relay/identity_cache.go:106 — a read started before a deactivate can put() after the invalidate, granting up to TTL past the put (bounded by TTL + one read latency; same call would succeed uncached). A generation counter would close it; not worth the complexity at 3s.
- internal/db/db.go:141-143 — reader-pool PRAGMAs mmap_size/cache_size run via reader.Exec and apply to one pooled conn only. Left as-is (more per-conn cache on a swapping host = more memory); follow-up candidate.
- tasks writes now maintain one more index (~65-90 pages); negligible vs the scan pages saved.

## 3. Files changed

```
internal/db/db.go                         |  84 ++++++++++++++++---
 internal/db/db_writerwait_test.go         |  48 +++++++++++
 internal/db/identity_read_lock_test.go    |  46 ++++++++++
 internal/db/referential_scan_plan_test.go |  84 +++++++++++++++++++
 internal/relay/handlers.go                |   4 +
 internal/relay/handlers_agents.go         |   3 +
 internal/relay/identity_cache.go          | 109 ++++++++++++++++++++++++
 internal/relay/identity_cache_test.go     | 135 ++++++++++++++++++++++++++++++
 internal/relay/toolset.go                 |   4 +-
 9 files changed, 503 insertions(+), 14 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `33dab33d-a093-49bf-b7de-e68b34d9ead2`._
