# [relay/memory] T2: set_memory based_on arg with read-cache auto-fill and author notice

## Team : wraith-engine (tsukumo)
## Branch : wraith/memory-t2 (from main)
## Relay task : 1a031d43-86eb-4bbb-8f8e-06f48e3006cd
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. set_memory accepts optional based_on; an explicit mismatch returns conflict:true, conflict_with, current_author, and sends the current author a relay message (type=fyi P2 by default; a normal P1 message when the key is in the constraints or decision layer); no notice on self-race or scope=agent (tests ExplicitMismatchSiblingAndNotice, ConstraintsLayerNoticeIsP1, SelfRaceNoNotice, AgentScopeNoNotice).
- [ ] 2. After get_memory returns exactly one live row, a later set_memory of that key by the same agent auto-fills based_on from an in-memory cache; with no read and no arg the result is causal:none and behaviour is today's LWW (tests AutofillFromGetMemory, NoReadNoArgLegacy).
- [ ] 3. get_memory performs no DB write (writer tx count unchanged); the cache is bounded by size and TTL; a fresh Handlers (restart) degrades to causal:none, never to a false conflict (tests GetMemoryDoesNoDBWrite, CacheBoundedAndTTL, RestartDegradesToLegacyNotConflict).
- [ ] 4. Each set_memory emits one [memory] causal=<arg|cache|none> outcome=<fast-forward|sibling|fresh|noop> log line and event:memory-conflict goes to the notifier on every sibling (tests CausalLogLine, ConflictEventEmitted).
- [ ] 5. TestToolSchemaBudget green; go vet clean; go test -tags fts5 -race ./internal/relay/... green.

## 2. Root cause & decisions

# 1a031d43 — set_memory based_on arg, read-cache auto-fill, conflict routing (DEC-wraith-memory-causal-1 T2)

ROOT_CAUSE: T1 (c4956f8) made the DB layer causal (SetMemoryWith / SetMemoryOpts.BasedOn: a stale writer becomes a live sibling instead of archiving the newer row), but no caller passed context, so the lost update of design §1.2 still happened on every MCP write, and nobody was told about a conflict.

DECISION:
- tools.go: optional based_on on set_memory (description trimmed for the schema budget).
- handlers_memory.go:
  - explicit based_on wins; else auto-fill from the read cache; else causal=none (legacy LWW);
  - explicit ErrBasedOnMismatch -> INVALID_ARGUMENT, nothing written; a stale cached id retries as causal=none (never refused, never a false conflict);
  - one "[memory] causal=<arg|cache|none> outcome=<fast-forward|sibling|fresh|noop>" log line per write;
  - on a sibling: conflict_with, current_author and current_value preview in the result, and event:memory-conflict emitted.
  - When the sibling comes from a based_on mismatch (not upsert=false, not a self-race, not scope=agent), the current author gets a relay message: fyi P2 action none by default, notification P1 action decide when either row is constraints/decision layer (OQ4 amendment).
  - HandleGetMemory records the id when exactly one row comes back.
- memory_readcache.go: mutex map keyed (project, agent, scope, key), zero value usable, 10k entries / 24h TTL, oldest evicted, no DB access.
- handlers.go: one field (memReads) on Handlers. 6th file vs the ticket's 5: the cache must live on Handlers; the zero value keeps NewHandlers untouched.

OUT honoured: no REST body change, no upsert default change, no auto-fill from search_memory/session_context.

## review-wraith verdict: SHIP
Scope: internal/relay/{tools.go,handlers_memory.go,memory_readcache.go,handlers.go,handlers_memory_causal_test.go}.
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/relay/... 0 FAIL (2 runs), ./internal/... 0 FAIL; TestToolSchemaBudget green.

BLOCKERS: none.
- No write on read: get_memory only touches the in-memory cache (GetMemoryDoesNoDBWrite via PRAGMA data_version on a second connection).
- Single writer: the compare-and-write stays in T1's BEGIN IMMEDIATE tx; the notice is one message insert per conflict, not per read.
- Non-destructive: a sibling keeps both rows live; nothing archived on mismatch.

NITS (non-blocking):
- The conflict notice is inserted without a push Notify (same as announceClaimable); a P1 decide notice still wakes through the normal unread path.
- Eviction scans the map when full (O(n) at 10k, only on insert into a full cache).

## 3. Files changed

```
internal/relay/handlers.go                    |   3 +
 internal/relay/handlers_memory.go             |  83 ++++++-
 internal/relay/handlers_memory_causal_test.go | 299 ++++++++++++++++++++++++++
 internal/relay/memory_readcache.go            |  86 ++++++++
 internal/relay/tools.go                       |   1 +
 5 files changed, 471 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `1a031d43-86eb-4bbb-8f8e-06f48e3006cd`._
