# [relay/memory] T1: SetMemory causal compare on based_on (db layer)

## Team : wraith-backend (tsukumo)
## Branch : feat/memory-causal-based-on (from main)
## Relay task : df33d619-461a-48fe-a007-f333eff735b0
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. With based_on=v3 while v4 is live, the write leaves v4 live and inserts a row with conflict_with=v4 and supersedes=v3; GetMemory returns both rows (tests LostUpdateReproBecomesSibling, BasedOnNewWithLiveIsSibling).
- [ ] 2. based_on equal to the live id archives and inserts exactly as today; a write without based_on produces rows byte-identical to current code; upsert=false is unchanged; a based_on naming another key/scope is rejected with nothing written (tests FastForwardArchivesAsToday, NoBasedOnUnchanged, UpsertFalseUnchanged, BasedOnWrongKeyRejectedNothingWritten).
- [ ] 3. On fast-forward supersede and on resolve_conflict, the archived predecessor gets valid_until equal to the successor's valid_from unless an earlier explicit valid_until exists; historical rows are not touched (tests H1PredecessorValidUntilEqualsSuccessorValidFrom, H1EarlierExplicitExpiryKept).
- [ ] 4. Same value with unchanged tags/confidence/layer only touches updated_at; same value with changed tags, confidence or layer creates a new version and archives the old row with its old metadata (tests H2SameValueSameMetaTouchesOnly, H2SameValueNewTagsVersions, H2LayerChangeNoLongerDropped).
- [ ] 5. Two goroutines writing with the same based_on yield exactly one fast-forward and one sibling (test ConcurrentWritersSerialize); go vet clean; go test -tags fts5 -race ./internal/db/... green; no schema change.

## 2. Root cause & decisions

## review-wraith verdict: SHIP
Scope: internal/db/memories.go (SetMemoryOpts/SetMemoryWith/checkBasedOnTx, decision table, H1 closeValidityClause in upsert + both ResolveConflict archive UPDATEs, H2), internal/db/memories_causal_test.go (new, 12 subtests). Task df33d619, ruling DEC-wraith-memory-causal-1, design trovex 06b42070041f474bb2f7b941b30d1aca §4 T1.
Gate: build -tags fts5 OK / vet (tags + no tags) OK / gofmt OK (touched files) / go test -tags fts5 -race ./internal/db/... ./internal/relay/... OK / go test -tags fts5 ./... OK

ROOT_CAUSE: SetMemory's upsert branch archived whatever row was live at write time with no notion of what the writer had read, so a writer working from v3 silently archived a concurrent v4 (19/443 upsert supersedes in prod were by a different agent within 24h, 13 on constraints). Adjacent: the supersede never closed the predecessor's valid_until (H1), and the same-value path overwrote tags/confidence in place and silently dropped a layer change (H2).
DECISION: SetMemory stays the exact legacy signature and becomes a wrapper over SetMemoryWith(..., upsert, SetMemoryOpts{BasedOn}), so the 3 callers (handlers_memory.go, api.go, decisions.go) are unchanged. In the existing BEGIN IMMEDIATE writer tx: based_on is validated first (same project/scope/key, same agent for agent scope; unknown or foreign id -> ErrBasedOnMismatch, tx rolled back, nothing written); then one switch: no live row -> v1; same value+tags+confidence+layer -> touch updated_at only; differing value + upsert=false -> conflict mode (unchanged); differing value + based_on set and != live id (or "new") -> SIBLING (live row untouched, conflict_with=live, supersedes=based_on / nil for "new"); otherwise supersede (archive + insert, as before) — which now also covers same value with changed metadata (H2). Every archive UPDATE (upsert, ResolveConflict losers, ResolveConflict all-archive) closes validity with valid_until = CASE WHEN NULL OR later THEN now ELSE keep END, forward only (OQ3: no backfill). One INSERT statement for all paths (conflict_with NULL where it was absent before — same stored value).
REJECTED: reject-on-mismatch (ruling OQ1 = sibling); a based_on version number (ids are unique across scopes, versions are not); changing SetMemory's signature (would touch 3 callers + REST, out of T1 scope); stamping ResolveConflict losers at the winner's valid_from (the loser WAS valid during the conflict window — they close at the resolution instant, which equals the new resolution row's valid_from on that path).

AC1: LostUpdateReproBecomesSibling, BasedOnNewWithLiveIsSibling.
AC2: FastForwardArchivesAsToday, NoBasedOnUnchanged (field-for-field legacy; the only extra write is H1 on the archived predecessor, which AC3 requires), UpsertFalseUnchanged, BasedOnWrongKeyRejectedNothingWritten (other key, unknown id, other scope, other agent's agent-scope row).
AC3: H1PredecessorValidUntilEqualsSuccessorValidFrom (incl. resolve_conflict), H1EarlierExplicitExpiryKept (earlier kept, later pulled in).
AC4: H2SameValueSameMetaTouchesOnly, H2SameValueNewTagsVersions, H2LayerChangeNoLongerDropped.
AC5: ConcurrentWritersSerialize (2 goroutines, same based_on -> 1 fast-forward + 1 sibling, both values live); race suite green; no schema change.

Thesis checks: single writer tx unchanged (beginWriterTx), no new writer, no read-path write (get/search untouched), no schema/migration change, agentColumns untouched, inbox/deliveries untouched.

BLOCKERS: none

NITS:
- UNCERTAIN (AC2 wording): "a write without based_on produces rows byte-identical" read together with AC3/AC4 — H1 (predecessor valid_until) and H2 (metadata-change versioning) apply regardless of based_on by design; everything else is identical. Flagged, not hidden.
- H2 compares tags as stored strings: MCP always sends db.TagsToJSON (canonical), but REST POST /api/memories passes the raw body tags JSON, so a whitespace-only difference there would create a version instead of a touch. Harmless (extra history row); canonicalise in T2 if it matters.

## 3. Files changed

```
internal/db/memories.go             | 249 ++++++++++++++-------------
 internal/db/memories_causal_test.go | 326 ++++++++++++++++++++++++++++++++++++
 2 files changed, 461 insertions(+), 114 deletions(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `df33d619-461a-48fe-a007-f333eff735b0`._
