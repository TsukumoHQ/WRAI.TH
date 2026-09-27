# [release] v1.23.2 release notes in CHANGELOG.md (v1.23.1..S2-fix merge)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/v1.23.2-notes (from main)
## Relay task : d81189d0-08a2-4bb7-8180-ac58f086834a
## Trace : trace=fe4ad10929ff1383c966fee4885478ab
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. CHANGELOG.md has exactly one '## [1.23.2] — <date>' heading, placed above '## [1.23.1]'
- [ ] 2. every non-release commit in v1.23.1..<S2-fix merge sha> is referenced by its sha7 in the [1.23.2] section, including 89a4ae7 and the S2 fix; 9e0aa38 is not listed
- [ ] 3. every ticket reference uses the task:xxxxxxxx form; no bare 8-hex id sits next to a sha7
- [ ] 4. git diff origin/main touches CHANGELOG.md only

## 2. Root cause & decisions

ROOT_CAUSE: v1.23.2 is being cut (ruling c46bee4c) once the exceptions S2 fix merged, and CHANGELOG.md had no section for the commits since v1.23.1.

DECISION: one [1.23.2] section above [1.23.1], range pinned at claim to v1.23.1..c7eed0d (confirmed by wraith-cto). The two user-visible commits go under ### Fixed, one bullet each: 89a4ae7 (task:3b919eb6), where batch_dispatch_tasks now honours per-item backlog and announces each item like dispatch_task, and c7eed0d (task:c6f6c5e3), where a failed exception write no longer blocks delivery expiry or lease release. 9e0aa38 (the v1.23.1 backfill) is a release chore and is not listed. Upgrade notes: no schema, tool or argument change. The section follows the pattern of 42eb2bb (lead paragraph, typed sections, `(task:xxxxxxxx, sha7)` refs).

REJECTED: a ### Changed entry for the batch announce parity. It is the same fix as the backlog bug (one shared announceDispatched helper), so it is folded into that single bullet.

## review-wraith verdict: SHIP
Scope: CHANGELOG.md only (+18, one [1.23.2] section above [1.23.1]); no Go code, no schema, no tool change.
Gate: build -tags fts5 OK / vet -tags fts5 OK / gofmt n/a (no .go touched) / test n/a (doc-only diff)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none. Claims checked against the code: announceDispatched is shared by batch and single dispatch (handlers_tasks.go:271); bestEffortExceptionTx uses SAVEPOINT / ROLLBACK TO / RELEASE and logs source_kind + source_ref (exceptions.go:144-153). Refs: task:3b919eb6 with 89a4ae7, task:c6f6c5e3 with c7eed0d; 9e0aa38 not listed.

## 3. Files changed

```
CHANGELOG.md | 18 ++++++++++++++++++
 1 file changed, 18 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `d81189d0-08a2-4bb7-8180-ac58f086834a`._
