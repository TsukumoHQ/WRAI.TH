# [wraith/relay] delete_memory loud-fail: targeted delete matching no row returns error naming key+author

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/delete-memory-loud-fail (from main)
## Relay task : 7d0629c7-f983-4ae6-9f22-99779dbbc228
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: delete_memory with explicit `agent` param matching no row (wrong author or missing key) returns an error whose message names both the key and the target author
- [ ] 2. AC2 named test: self-delete (no `agent` param) of a missing key keeps current lenient behavior, unchanged

## 2. Root cause & decisions

ROOT_CAUSE: DeleteMemoryAs (internal/db/memories.go) returned a single generic "memory not found: <key> (scope=<scope>)" error on RowsAffected==0 for BOTH self-delete and the targeted cross-author admin/janitor arm. When an operator targets a specific author via the explicit `agent` param and the assumed author is wrong (live incident: frontend-lead-resume was authored by anonymous, DEV-checkpoint by dev), the no-op error did not name the author, so the operator could not tell WHICH target matched nothing and believed the purge done. FIX: in the n==0 branch, when the delete is a targeted cross-author agent-scope delete (scope=='agent' && !EqualFold(targetAuthor, actingAgent)), return an error naming BOTH the key and the target author. The self-delete path (targetAuthor==actingAgent, including no `agent` param) keeps the exact prior message — byte-identical, no behavior change.

## review-wraith verdict: SHIP
Scope: internal/db/memories.go (DeleteMemoryAs n==0 branch), internal/db/delete_memory_loud_fail_test.go (new)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (829 pass, full ./...)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- No schema/migration, no new writer (reuses the existing single UPDATE via writerExec), single-writer + WAL untouched. Backward-compatible: the successful-delete path and the self-delete not-found message are unchanged; only the cross-author no-op error text is enriched. No caller parses the error string (handler wraps it as toolResultError), so the message change is safe. Failure-is-loud satisfied: AC1 asserts the exact message names key + author.

## 3. Files changed

```
internal/db/delete_memory_loud_fail_test.go | 68 +++++++++++++++++++++++++++++
 internal/db/memories.go                     | 11 +++++
 2 files changed, 79 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `7d0629c7-f983-4ae6-9f22-99779dbbc228`._
