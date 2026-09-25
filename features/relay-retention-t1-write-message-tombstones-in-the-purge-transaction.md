# [relay/retention] T1: write message_tombstones in the purge transaction

## Team : wraith-engine (tsukumo)
## Branch : wraith/tombstones-t1 (from main)
## Relay task : f53b2160-5e82-48b0-adab-59c3677fae0e
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. PurgeExpiredMessages writes one message_tombstones row per purged message in the same transaction, before the delete, with fields equal to the source row (test PurgeWritesOnePerPurgedRow).
- [ ] 2. message_tombstones has no subject, content or metadata column (test NoContentColumns via PRAGMA table_info).
- [ ] 3. A second purge adds 0 rows without error; if the tombstone insert fails, neither tombstone nor delete lands (tests IdempotentRePurge, TxAtomic with forced error).
- [ ] 4. P0 and ttl=0 messages are never purged and never tombstoned; inbox/unread results are identical with the table populated; a DB without the table gains it and a second migrate is a no-op (tests P0AndTTL0NeverTombstoned, InboxUnchanged, OldDBMigrates).
- [ ] 5. go vet ./... clean; go test -tags fts5 -race ./internal/db/... ./internal/relay/... green.

## 2. Root cause & decisions

# f53b2160 — message_tombstones written in the purge tx (DEC-wraith-tombstones-1 T1)

ROOT_CAUSE: PurgeExpiredMessages hard-deleted soft-expired messages and left no trace of the id, so a purged reply_to parent and a mistyped id were indistinguishable (16/37 prod reply_to dangle; R1 reject-on-unknown undecidable). Design: wraith-cto/designs/a83630d7-message-tombstones.md sections 2 + 4.

DECISION:
- migrate creates message_tombstones (id PK, project, from_agent, to_agent, type, reply_to, task_id, trace_id, action_required, priority, created_at, purged_at) WITHOUT ROWID + partial index on reply_to. No subject/content/metadata. Additive, CREATE IF NOT EXISTS.
- PurgeExpiredMessages: INSERT OR IGNORE ... SELECT with the exact purge predicate, inside the existing writer tx, before the deliveries/reads/messages deletes. Tombstone and delete land together or not at all. All purged ids tombstoned (OQ1); kept forever (OQ2).
- PurgeExpiredMessages signature and return value unchanged (it wraps the new PurgeExpiredMessagesWithTombstones). The cleanup tick calls the latter only to log "N tombstone(s) written".
- P0 / ttl=0 are never soft-expired, so never purged, so never tombstoned (test keeps that retention promise).

REJECTED: changing PurgeExpiredMessages' return (4 callers incl. tests); a separate COUNT query for the log (extra read, same number the INSERT already reports).

SCHEMA: one new table + index, additive. An old binary on a new DB ignores the table.

## review-wraith verdict: SHIP
Scope: internal/db/db.go (table + index), internal/db/messages.go (purge tx), internal/relay/cleanup.go (log line), internal/db/messages_tombstone_test.go.
Gate: build -tags fts5 OK / vet OK / gofmt OK / go test -count=1 -tags fts5 -race ./internal/db/... ./internal/relay/... green (2 runs).

BLOCKERS: none.
- Single writer: the INSERT runs in the existing beginWriterTx of the once-per-tick purge; no new writer, no hot-path write.
- Non-destructive inbox: no inbox/wake/delivery query reads message_tombstones (TestTombstone/InboxUnchanged).
- Migration idempotent (TestTombstone/OldDBMigrates runs migrate twice on a DB without the table).

NITS (non-blocking):
- The INSERT ... SELECT re-evaluates the purge predicate a 4th time in the tx (datetime() on expired_at); fine at ~48-154 rows/day, the existing idx_messages_expired covers it.

## 3. Files changed

```
internal/db/db.go                      |  20 +++
 internal/db/messages.go                |  35 ++++-
 internal/db/messages_tombstone_test.go | 254 +++++++++++++++++++++++++++++++++
 internal/relay/cleanup.go              |   4 +-
 4 files changed, 304 insertions(+), 9 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `f53b2160-5e82-48b0-adab-59c3677fae0e`._
