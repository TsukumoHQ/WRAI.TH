# [relay/linkage] slice B: optional task_id arg on send_message, soft reply_to resolution

## Team : wraith-engine (tsukumo)
## Branch : wraith/linkage-b (from main)
## Relay task : fde40c25-fc76-4439-b595-aa3b087d7e8e
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. send_message accepts an optional task_id; a task_id that exists in the project is stored (via metadata promotion from slice A); an unknown task_id is rejected before insert with 'unknown task_id' and nothing is persisted; a task_id that differs from metadata.task_id is rejected with 'task_id conflicts with metadata.task_id' (tests TaskIDArgStored, UnknownTaskIDRejectedNothingPersisted, TaskIDConflictRejected).
- [ ] 2. A reply_to that is not a well-formed message id is rejected before insert (test MalformedReplyToRejected).
- [ ] 3. A well-formed reply_to whose parent does not exist is accepted and stored as given, nothing is derived from it, the send result contains reply_to_resolved:false, and one [linkage] log line is emitted (test DanglingReplyToAcceptedUnresolved).
- [ ] 4. A send with neither task_id nor reply_to returns the same result shape as before plus no new fields (test NoArgBehaviourUnchanged); cross-project reply_to on sendCrossProject is not validated.
- [ ] 5. go vet ./... clean; go test -tags fts5 -race ./internal/... green.

## 2. Root cause & decisions

# fde40c25 — send_message task_id arg + soft reply_to (DEC-wraith-linkage-1 slice B)

ROOT_CAUSE: senders had no way to declare which task a message is about (links lived only in prose), and reply_to was stored unchecked: a typo'd id and a TTL-purged parent looked identical and both silently linked nothing (16/37 prod reply_to dangle).

DECISION:
- send_message optional task_id: must resolve to a task in the project (GetTask) and must not differ from a metadata.task_id already set; otherwise refused BEFORE insert ('unknown task_id', 'task_id conflicts with metadata.task_id'). Merged into metadata; slice A's deriveTaskID promotes it. No DB signature change.
- task_id on cross-project / federated sends is refused explicitly (the task would be looked up in the wrong project and silently dropped).
- reply_to (OQ1 amended): not a 36-char UUID -> refused; well-formed but parent absent from the project -> accepted, stored as given, nothing derived, result carries reply_to_resolved:false, one [linkage] log line. Resolved -> reply_to_resolved:true. No reply_to -> result shape unchanged. Same rule on POST /user-response (400 on malformed).
- Cross-project (sendCrossProject) and federated reply_to are not validated (the parent lives elsewhere).

REJECTED: hard reject on unknown reply_to (ruled out until tombstones exist); a task_id param on InsertMessage* (9 callers; metadata already carries it); REST /messages body fields (OUT).

SCHEMA: none.

## review-wraith verdict: SHIP
Scope: internal/relay/tools.go (schema), internal/relay/handlers_messaging.go (task_id/reply_to checks, sendResult), internal/relay/api.go (W9), internal/relay/handlers_linkage_test.go.
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/... = 932 passed; rebased on 4cbaa4c (leaked-flusher race fix), full suite green twice.

BLOCKERS: none.
- Handler-only; no new writer, no DB write added; 2 RO reads (GetTask/GetMessage) only when the args are present.
- Nothing is dropped silently: every refusal happens before insert and returns an error to the sender; a dangling reply_to is accepted.
- Additive tool arg; plain send result byte-shape unchanged (TestSendLinkage/NoArgBehaviourUnchanged).

NITS (non-blocking):
- Malformed reply_to used to be accepted silently; now refused. Prod has 0 non-UUID reply_to values, so no known caller breaks.
- A dangling response still derives action_required 'do' via the existing legacy-parent fallback (unchanged behaviour, not linkage).

## 3. Files changed

```
internal/relay/api.go                   |  11 +-
 internal/relay/handlers_linkage_test.go | 202 ++++++++++++++++++++++++++++++++
 internal/relay/handlers_messaging.go    |  83 ++++++++++++-
 internal/relay/tools.go                 |   3 +-
 4 files changed, 295 insertions(+), 4 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `fde40c25-fc76-4439-b595-aa3b087d7e8e`._
