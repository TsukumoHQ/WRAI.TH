# [relay/retention] T2: resolve message refs through tombstones, log-only reply_to probe

## Team : wraith-engine (tsukumo)
## Branch : wraith/tombstones-t2 (from main)
## Relay task : 93b6f1cf-6806-4ef1-8f67-5d011189bf4a
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. ResolveMessageRef(project, id) returns live, tombstoned or unknown; an id from another project is unknown (tests Live, Tombstoned, Unknown, CrossProjectIsUnknown).
- [ ] 2. A response whose reply_to parent is tombstoned inherits the parent's action_required (ask, not the do fallback) and trace_id (tests ReplyToTombstonedParentInheritsActionTag, ReplyToTombstonedParentInheritsTraceID).
- [ ] 3. On local send paths a reply_to that is not live emits one [linkage] reply_to=<id> state=<unknown|tombstoned> log line; the send still succeeds and its delivery row exists; the send_message result still reports reply_to_resolved as today (test ProbeLogsNeverRejects).
- [ ] 4. go vet clean; go test -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

# 93b6f1cf — resolve message refs through tombstones, log-only reply_to probe (DEC-wraith-tombstones-1 T2)

ROOT_CAUSE: after the TTL purge, a reply_to parent vanished: deriveActionRequired fell back to "do" (a reply to a purged question woke as a task), deriveTraceID lost the causal trace, and slice B's resolveReplyTo could not tell a purged parent from an unknown id. T1 (c6add48) now leaves a tombstone; nothing read it.

DECISION:
- db.ResolveMessageRef(project, id) -> live | tombstoned | unknown: messages then message_tombstones, same project, RO pool. Another project's id is unknown.
- deriveActionRequired / deriveTraceID: when the parent row is absent (sql.ErrNoRows) read action_required / trace_id from its tombstone. A live parent with a NULL tag keeps the old legacy "do" path.
- resolveReplyTo (send_message W1-W3 and POST /user-response W9) uses ResolveMessageRef: tombstoned counts as resolved (reply_to_resolved:true), unknown stays false. Every non-live ref logs one "[linkage] reply_to=<id> state=<unknown|tombstoned> project=<p> from=<a>" line. Never rejects (OQ3: reject-on-unknown waits for one live retention window + 0 unknown probes).
- The slice-B log line is replaced by the probe line; handlers_linkage_test.go DanglingReplyToAcceptedUnresolved now asserts the new string (still exactly one line).

SCHEMA: none.

## review-wraith verdict: SHIP
Scope: internal/db/messages.go, internal/relay/handlers_messaging.go, tests internal/db/messages_resolve_test.go, internal/relay/handlers_linkage_probe_test.go, internal/relay/handlers_linkage_test.go (one expected string).
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... 0 FAIL (2 runs).

BLOCKERS: none.
- Reads only (RO pool); at most one extra RO lookup per reply, and only when the parent row is gone. No writer change.
- Non-destructive: the probe never rejects; the send and its delivery row land (ProbeLogsNeverRejects).

NITS (non-blocking):
- deriveTaskID (linkage slice A) does not yet inherit task_id from a tombstoned parent, although the tombstone carries it; out of this ticket's AC, a one-line follow-up.
- Tombstoned => reply_to_resolved:true is covered at the DB layer (TestResolve/Tombstoned); a handler-level test would need a purge helper in the relay test package.

## 3. Files changed

```
internal/db/messages.go                       |  35 ++++++++-
 internal/db/messages_resolve_test.go          | 106 ++++++++++++++++++++++++++
 internal/relay/handlers_linkage_probe_test.go |  55 +++++++++++++
 internal/relay/handlers_linkage_test.go       |   2 +-
 internal/relay/handlers_messaging.go          |  13 ++--
 5 files changed, 203 insertions(+), 8 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `93b6f1cf-6806-4ef1-8f67-5d011189bf4a`._
