# [release] v1.23.0 release notes in CHANGELOG.md (v1.22.0..d78ba21)

## Team : wraith-backend (tsukumo)
## Branch : wraith-backend/release-1.23 (from main)
## Relay task : 0fbac063-3af0-4fa3-92c8-7eba9d319c49
## Trace : trace=07bc620dfecd004d4ce999c37c02f25c
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 CHANGELOG.md has exactly one '## [1.23.0] — <date>' heading, placed above '## [1.22.0]'
- [ ] 2. AC2 every non-release commit in v1.22.0..d78ba21 is referenced by its sha7 in the [1.23.0] section, including guards S2b d78ba21; release-to-pending 545a11d2 is NOT listed
- [ ] 3. AC3 every ticket reference uses the task:xxxxxxxx form; no bare 8-hex id sits next to a sha7
- [ ] 4. AC4 git diff origin/main touches CHANGELOG.md only

## 2. Root cause & decisions

# [release] v1.23.0 release notes in CHANGELOG.md (v1.22.0..d78ba21)

Task: 0fbac063-3af0-4fa3-92c8-7eba9d319c49

ROOT_CAUSE: main was 28 commits past v1.22.0 with no notes. Founder policy (ruling c6912e1a) is that every deploy is tagged and main is never more than 10 commits past a tag.

DECISION:
- One `## [1.23.0] — 2026-09-27` section above `## [1.22.0]`, following the pattern of c8141e7: a one-paragraph lead, then Added / Changed / Fixed / Upgrade notes.
- Range: v1.22.0..d78ba21, per rulings c6912e1a and 3fb1c3c4. Guards S2b (d78ba21) is included. 545a11d2 is excluded and goes in v1.23.1. 9f4c742 (the changelog history rebuild) is a release commit, so it gets no bullet.
- Every one of the other 27 commits has one bullet ending `(task:xxxxxxxx, sha7)`. Task ids come from each commit's features/*.md `Relay task` line; S2b uses its resubmit id d17673fc, as the ruling says.
- The two tool-schema reclaims and the two settings declarations are listed under Changed, one line each.
- The Upgrade notes come from the code at d78ba21, not from the records:
  - new tables and views: the CREATE statements in internal/db;
  - new column: the ensureColumns calls;
  - setting defaults: internal/relay/settings_spec.go and internal/db/contradictions.go.

VERIFY:
- `grep -c '^## \[1.23.0\]' CHANGELOG.md` prints 1.
- `go build ./...` passes.
- Every non-release sha in v1.22.0..origin/main appears in the section (scripted check: 0 missing).
- `git diff --stat origin/main` shows CHANGELOG.md only.

## review-wraith verdict: SHIP
Scope: CHANGELOG.md only (+47 lines, new [1.23.0] section). No code, schema, tool-schema, migration, handler or updater change.
Gate: build -tags fts5 OK / vet OK / gofmt N/A (no Go touched) / test N/A (docs-only diff, no Go touched).
Checks:
- Every non-release sha7 in v1.22.0..d78ba21 appears in the section: 27 commits, 0 missing.
- Every ref has the form `(task:xxxxxxxx, sha7)`, and no bare 8-hex id sits next to a sha.
- The Upgrade notes' tables, column and settings defaults were checked against the source at d78ba21.
- No tag or release was cut by the doer; wraith-cto tags v1.23.0 on the merge sha.

BLOCKERS: none
NITS:
- Task 0fbac063 still carries blocked_by edges to 545a11d2 and d17673fc, so readiness reports ready=false. d17673fc is merged, and 545a11d2 was moved to v1.23.1 by ruling c6912e1a, so the 545a11d2 edge is stale.

## 3. Files changed

```
CHANGELOG.md | 47 +++++++++++++++++++++++++++++++++++++++++++++++
 1 file changed, 47 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `0fbac063-3af0-4fa3-92c8-7eba9d319c49`._
