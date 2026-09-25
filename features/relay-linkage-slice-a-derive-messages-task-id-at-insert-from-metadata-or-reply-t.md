# [relay/linkage] slice A: derive messages.task_id at insert from metadata or reply_to parent

## Team : wraith-engine (tsukumo)
## Branch : wraith/linkage-a (from main)
## Relay task : da1945d5-5e13-4898-8a89-0e6e6ac9d4cd
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. Both DB sinks (internal/db/messages.go:151 and :250) set task_id from a non-empty metadata.task_id that resolves to a task in the same project; an empty, unknown or cross-project value leaves task_id NULL and the insert and its delivery rows still succeed (tests MetadataTaskIDPromoted, EmptyMetadataTaskIDStaysNull, UnknownOrCrossProjectTaskIDStaysNull).
- [ ] 2. With no metadata.task_id, a message whose reply_to parent is in the same project inherits the parent's task_id; a cross-project parent is not inherited; when metadata.task_id and the inherited value both exist and differ, metadata.task_id wins and one [linkage] conflict log line is emitted (tests ReplyInheritsParentTaskID, CrossProjectParentNotInherited, MetadataWinsOverInheritedWithConflictLog).
- [ ] 3. A migration backfills task_id WHERE task_id IS NULL with the same predicate; running it twice changes nothing the second time (test BackfillIdempotent).
- [ ] 4. The boot selection journal line (from 409fb2e8) carries, per delivered message, whether task_id is set; get_inbox and get_session_context output are otherwise unchanged (test InboxUnchanged).
- [ ] 5. go vet ./... clean; go test -tags fts5 -race ./internal/... green.

## 2. Root cause & decisions

# da1945d5 — derive messages.task_id at insert (DEC-wraith-linkage-1 slice A)

ROOT_CAUSE: messages.task_id existed (db.go ensureColumns, idx_messages_task) and was read back everywhere, but neither DB sink (InsertMessage, InsertMessageWithDeliveries) ever wrote it. 43 relay-written metadata.task_id values (announceClaimable + notifier) resolved 43/43 to same-project tasks while the column was 0/620, so the message->task graph that supersession/packets/replay need did not exist in data (design trovex fb4cb94f57854262bd5e7dfb4323c5d2).

DECISION:
- deriveTaskID (messages.go, next to deriveTraceID, same metadata parse + RO lookups): a non-empty metadata.task_id that resolves to a task in the same project wins; else the same-project reply_to parent's task_id is inherited; else NULL. Metadata vs inherited conflict -> metadata wins + one "[linkage] task_id conflict" log line (amendment a). Never inferred from prose. Never errors the insert.
- Both INSERTs write task_id; the returned models.Message carries TaskID.
- backfillMessageTaskIDs (db.go migrate): same metadata predicate on task_id IS NULL rows, every boot, idempotent (second run = 0 rows).
- budgetScore.has_task on both [budget] journal paths (amendment b); projection unchanged.

REJECTED: prose/8-hex inference (precision rule); a settings-marker one-shot for the backfill (the IS NULL predicate is already idempotent and a no-op after the first boot); a task_id param on InsertMessage* (9 callers; metadata already carries it).

SCHEMA: none. Column + index pre-exist. Additive, older binaries unaffected.

## review-wraith verdict: SHIP
Scope: internal/db/messages.go (deriveTaskID, both INSERTs), internal/db/db.go (backfillMessageTaskIDs + migrate call), internal/relay/budget.go + project.go (has_task), tests internal/db/messages_linkage_test.go, internal/relay/project_journal_test.go.
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/... = 922 passed.

BLOCKERS: none.
- No new writer; derivation = 2 RO-pool reads per insert, same shape as deriveTraceID (no new hot-path write). Backfill runs on the migrate writer at boot only.
- Non-destructive inbox: no inbox/wake/delivery query reads task_id (TestLinkage/InboxUnchanged; session_context golden byte-identical).
- Additive: no agentColumns/scanAgent touch, no ALTER.

NITS (non-blocking):
- deriveTaskID and deriveTraceID both read the reply_to parent row; could be one SELECT (trace_id, task_id). Left separate for clarity.
- File count 6 (4 non-test): budget.go touched for the has_task struct field, which amendment (b) needs; the ticket list named project.go only.
- Pre-existing race flake seen once in TestSettingsSecretHandling (leaked flushTokenUsage goroutine logging into a captured buffer, handlers.go:213); passes on rerun, not in this diff; .worktrees/flusher-leak exists.

## 3. Files changed

```
internal/db/db.go                      |  23 ++++
 internal/db/messages.go                |  42 ++++++-
 internal/db/messages_linkage_test.go   | 222 +++++++++++++++++++++++++++++++++
 internal/relay/budget.go               |   2 +
 internal/relay/project.go              |   2 +-
 internal/relay/project_journal_test.go |  16 +++
 6 files changed, 302 insertions(+), 5 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `da1945d5-5e13-4898-8a89-0e6e6ac9d4cd`._
